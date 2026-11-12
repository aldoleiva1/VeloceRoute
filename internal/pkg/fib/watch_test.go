// Copyright (C) 2026 VeloceRoute contributors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package fib

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	api "github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"
)

func freeTCPPort(t *testing.T) uint32 {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return uint32(l.Addr().(*net.TCPAddr).Port)
}

func localPath(t *testing.T, prefix, nh string) apiutil.AddPathRequest {
	t.Helper()
	n, err := bgp.NewIPAddrPrefix(netip.MustParsePrefix(prefix))
	if err != nil {
		t.Fatal(err)
	}
	o := bgp.NewPathAttributeOrigin(0)
	a, _ := bgp.NewPathAttributeNextHop(netip.MustParseAddr(nh))
	return apiutil.AddPathRequest{Paths: []*apiutil.Path{{
		Family: bgp.RF_IPv4_UC, Nlri: n, Attrs: []bgp.PathAttributeInterface{o, a},
	}}}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Two real BGP servers on loopback: "a" originates a prefix, "b" learns it and
// the syncer attached to b's best path events programs, then removes, the
// route. This exercises the real event path (Watch, apiutil paths, next hop
// attributes) that the fake-event tests above only imitate.
func TestWatchProgramsLearnedRoutesAndIgnoresLocalOnes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bPort := freeTCPPort(t)
	a := server.NewBgpServer()
	b := server.NewBgpServer()
	go a.Serve()
	go b.Serve()
	defer a.Stop()
	defer b.Stop()

	if err := a.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{Asn: 65001, RouterId: "1.1.1.1", ListenPort: -1}}); err != nil {
		t.Fatal(err)
	}
	if err := b.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{Asn: 65002, RouterId: "2.2.2.2", ListenPort: int32(bPort), ListenAddresses: []string{"127.0.0.1"}}}); err != nil {
		t.Fatal(err)
	}

	f := newFake()
	syn := NewSyncer(f, quiet())
	if err := syn.Watch(ctx, b); err != nil {
		t.Fatal(err)
	}

	// b originates its own prefix before any session exists: never programmed.
	if _, err := b.AddPath(localPath(t, "198.18.0.0/24", "10.99.0.2")); err != nil {
		t.Fatal(err)
	}

	if err := b.AddPeer(ctx, &api.AddPeerRequest{Peer: &api.Peer{
		Conf:      &api.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 65001},
		Transport: &api.Transport{PassiveMode: true},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := a.AddPeer(ctx, &api.AddPeerRequest{Peer: &api.Peer{
		Conf:      &api.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 65002},
		Transport: &api.Transport{RemotePort: bPort},
		Timers:    &api.Timers{Config: &api.TimersConfig{ConnectRetry: 1}},
	}}); err != nil {
		t.Fatal(err)
	}

	if _, err := a.AddPath(localPath(t, "192.0.2.0/24", "10.99.0.1")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "b to learn 192.0.2.0/24 and the syncer to program it", func() bool {
		return syn.Stats().Installed == 1
	})
	// GoBGP keeps the next hop that was given explicitly when the path was
	// injected, so b sees 10.99.0.1 and not the session address.
	if got := f.table[netip.MustParsePrefix("192.0.2.0/24")]; len(got) != 1 || got[0] != netip.MustParseAddr("10.99.0.1") {
		t.Fatalf("programmed next hop %v, want 10.99.0.1 (table: %s)", got, f.routes())
	}
	if _, ok := f.table[netip.MustParsePrefix("198.18.0.0/24")]; ok {
		t.Fatal("b's own locally originated prefix was programmed")
	}

	// a withdraws: the route must be removed.
	if err := a.DeletePath(apiutil.DeletePathRequest{Paths: localPath(t, "192.0.2.0/24", "10.99.0.1").Paths}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the withdrawn route to be removed", func() bool { return syn.Stats().Installed == 0 })
	if f.routes() != "" {
		t.Fatalf("route left after withdraw: %s", f.routes())
	}
}

// Three real BGP servers on loopback: b runs with multiple paths and learns the
// same prefix, with the same attributes, from a1 and a2. The syncer on b must
// build one ECMP route, shrink it when one path is withdrawn, and shrink it
// again when a session drops (the peer-down case, where GoBGP reports a
// withdrawal for every path of that peer).
func TestWatchMultipathBuildsAndShrinksEcmpRoute(t *testing.T) {
	if l, err := net.Listen("tcp", "127.0.0.2:0"); err != nil {
		t.Skipf("127.0.0.2 is not available for a second peer address: %v", err)
	} else {
		l.Close()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bPort := freeTCPPort(t)
	a1, a2, b := server.NewBgpServer(), server.NewBgpServer(), server.NewBgpServer()
	go a1.Serve()
	go a2.Serve()
	go b.Serve()
	defer a1.Stop()
	defer b.Stop()

	for _, a := range []struct {
		s   *server.BgpServer
		asn uint32
		id  string
	}{{a1, 65001, "1.1.1.1"}, {a2, 65003, "3.3.3.3"}} {
		if err := a.s.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{Asn: a.asn, RouterId: a.id, ListenPort: -1}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{
		Asn: 65002, RouterId: "2.2.2.2", ListenPort: int32(bPort), ListenAddresses: []string{"127.0.0.1"}, UseMultiplePaths: true,
	}}); err != nil {
		t.Fatal(err)
	}

	f := newFake()
	syn := NewSyncer(f, quiet(), WithMultipath(0))
	if err := syn.Watch(ctx, b); err != nil {
		t.Fatal(err)
	}

	for _, p := range []struct {
		addr string
		asn  uint32
	}{{"127.0.0.1", 65001}, {"127.0.0.2", 65003}} {
		if err := b.AddPeer(ctx, &api.AddPeerRequest{Peer: &api.Peer{
			Conf:      &api.PeerConf{NeighborAddress: p.addr, PeerAsn: p.asn},
			Transport: &api.Transport{PassiveMode: true},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range []struct {
		s     *server.BgpServer
		local string
	}{{a1, "127.0.0.1"}, {a2, "127.0.0.2"}} {
		if err := a.s.AddPeer(ctx, &api.AddPeerRequest{Peer: &api.Peer{
			Conf:      &api.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 65002},
			Transport: &api.Transport{RemotePort: bPort, LocalAddress: a.local},
			Timers:    &api.Timers{Config: &api.TimersConfig{ConnectRetry: 1}},
		}}); err != nil {
			t.Fatal(err)
		}
	}

	const prefix = "192.0.2.0/24"
	if _, err := a1.AddPath(localPath(t, prefix, "10.99.0.1")); err != nil {
		t.Fatal(err)
	}
	if _, err := a2.AddPath(localPath(t, prefix, "10.99.0.3")); err != nil {
		t.Fatal(err)
	}
	want := func(hops string) func() bool {
		return func() bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.routesLocked() == hops
		}
	}
	waitFor(t, "an ECMP route over both paths", want(prefix+">10.99.0.1+10.99.0.3"))

	// a1 withdraws its path: the route keeps a2's next hop only.
	if err := a1.DeletePath(apiutil.DeletePathRequest{Paths: localPath(t, prefix, "10.99.0.1").Paths}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the route to shrink to a2's next hop", want(prefix+">10.99.0.3"))

	// a1 announces again, then a2's session goes away.
	if _, err := a1.AddPath(localPath(t, prefix, "10.99.0.1")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the ECMP route again", want(prefix+">10.99.0.1+10.99.0.3"))
	a2.Stop()
	waitFor(t, "the route to shrink to a1's next hop after a2's session dropped", want(prefix+">10.99.0.1"))

	if err := a1.DeletePath(apiutil.DeletePathRequest{Paths: localPath(t, prefix, "10.99.0.1").Paths}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the route to be removed", want(""))
}

// The compact event and the API path are two spellings of the same facts, and
// classify must decide the same for both.
func TestClassifyChangeMatchesClassify(t *testing.T) {
	v4n, _ := bgp.NewIPAddrPrefix(netip.MustParsePrefix("192.0.2.0/24"))
	v6n, _ := bgp.NewIPAddrPrefix(netip.MustParsePrefix("2001:db8::/32"))
	evpn := bgp.NewEVPNEthernetAutoDiscoveryRoute(nil, bgp.EthernetSegmentIdentifier{}, 0, 0)
	hops := []string{"", "0.0.0.0", "::", "10.0.0.1", "2001:db8::1", "fe80::1"}
	peers := []netip.Addr{{}, netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("fe80::2%eth1"), netip.MustParseAddr("fe80::2")}
	n := 0
	for _, fam := range []bgp.Family{bgp.RF_IPv4_UC, bgp.RF_IPv6_UC, bgp.RF_EVPN} {
		for _, nlri := range []bgp.NLRI{v4n, v6n, evpn, nil} {
			for _, h := range hops {
				for _, pr := range peers {
					for _, wd := range []bool{false, true} {
						for _, bad := range []bool{false, true} {
							nh := netip.Addr{}
							var attrs []bgp.PathAttributeInterface
							if h != "" {
								nh = netip.MustParseAddr(h)
								if nh.Is4() {
									a, _ := bgp.NewPathAttributeNextHop(nh)
									attrs = append(attrs, a)
								} else {
									attrs = append(attrs, &bgp.PathAttributeMpReachNLRI{Nexthop: nh})
								}
							}
							p := &apiutil.Path{Family: fam, Nlri: nlri, Attrs: attrs, PeerAddress: pr, Withdrawal: wd, IsNexthopInvalid: bad}
							c := &server.BestPathChange{Family: fam, Nlri: nlri, NextHop: nh, PeerAddress: pr, Withdrawal: wd, IsNexthopInvalid: bad}
							pa, na, da := classify(p)
							pb, nb, db := classifyChange(c)
							if pa != pb || na != nb || da != db {
								t.Fatalf("%+v: classify %v %v %v, classifyChange %v %v %v", c, pa, na, da, pb, nb, db)
							}
							ea, eb := makeEvent(p), makeChangeEvent(c)
							if ea != eb {
								t.Fatalf("%+v: makeEvent %+v, makeChangeEvent %+v", c, ea, eb)
							}
							n++
						}
					}
				}
			}
		}
	}
	t.Logf("%d combinations", n)
}

// Two syncers watch the same server, one through the API paths and one
// through the compact events (with multiple paths, so the delta, withdraw and
// peer-down cases happen). Their tables must be the same at every step.
func TestCompactEventsGiveTheSameTableAsApiPaths(t *testing.T) {
	if l, err := net.Listen("tcp", "127.0.0.2:0"); err != nil {
		t.Skipf("127.0.0.2 is not available for a second peer address: %v", err)
	} else {
		l.Close()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bPort := freeTCPPort(t)
	a1, a2, b := server.NewBgpServer(), server.NewBgpServer(), server.NewBgpServer()
	go a1.Serve()
	go a2.Serve()
	go b.Serve()
	defer a1.Stop()
	defer b.Stop()
	for _, a := range []struct {
		s   *server.BgpServer
		asn uint32
		id  string
	}{{a1, 65001, "1.1.1.1"}, {a2, 65003, "3.3.3.3"}} {
		if err := a.s.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{Asn: a.asn, RouterId: a.id, ListenPort: -1}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{
		Asn: 65002, RouterId: "2.2.2.2", ListenPort: int32(bPort), ListenAddresses: []string{"127.0.0.1"}, UseMultiplePaths: true,
	}}); err != nil {
		t.Fatal(err)
	}

	fNew, fOld := newFake(), newFake()
	synNew := NewSyncer(fNew, quiet(), WithMultipath(0))
	synOld := NewSyncer(fOld, quiet(), WithMultipath(0))
	if err := synNew.Watch(ctx, b); err != nil {
		t.Fatal(err)
	}
	// The older way: full paths converted to API paths.
	go synOld.drain(ctx)
	if err := b.WatchEvent(ctx, server.WatchEventMessageCallbacks{
		OnBestPath: func(paths []*apiutil.Path, _ time.Time) { synOld.Submit(paths) },
	}, server.WatchBestPath(true)); err != nil {
		t.Fatal(err)
	}

	for _, p := range []struct {
		addr string
		asn  uint32
	}{{"127.0.0.1", 65001}, {"127.0.0.2", 65003}} {
		if err := b.AddPeer(ctx, &api.AddPeerRequest{Peer: &api.Peer{
			Conf:      &api.PeerConf{NeighborAddress: p.addr, PeerAsn: p.asn},
			Transport: &api.Transport{PassiveMode: true},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range []struct {
		s     *server.BgpServer
		local string
	}{{a1, "127.0.0.1"}, {a2, "127.0.0.2"}} {
		if err := a.s.AddPeer(ctx, &api.AddPeerRequest{Peer: &api.Peer{
			Conf:      &api.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 65002},
			Transport: &api.Transport{RemotePort: bPort, LocalAddress: a.local},
			Timers:    &api.Timers{Config: &api.TimersConfig{ConnectRetry: 1}},
		}}); err != nil {
			t.Fatal(err)
		}
	}

	same := func(what string, want string) {
		t.Helper()
		waitFor(t, what, func() bool { return fNew.routes() == want && fOld.routes() == want })
	}
	add := func(s *server.BgpServer, prefix, nh string) {
		t.Helper()
		if _, err := s.AddPath(localPath(t, prefix, nh)); err != nil {
			t.Fatal(err)
		}
	}
	del := func(s *server.BgpServer, prefix, nh string) {
		t.Helper()
		if err := s.DeletePath(apiutil.DeletePathRequest{Paths: localPath(t, prefix, nh).Paths}); err != nil {
			t.Fatal(err)
		}
	}
	add(a1, "192.0.2.0/24", "10.99.0.1")
	add(a2, "192.0.2.0/24", "10.99.0.3")
	add(a1, "198.51.100.0/24", "10.99.0.1")
	same("both paths in both tables", "192.0.2.0/24>10.99.0.1+10.99.0.3,198.51.100.0/24>10.99.0.1")
	del(a1, "192.0.2.0/24", "10.99.0.1")
	same("one path withdrawn", "192.0.2.0/24>10.99.0.3,198.51.100.0/24>10.99.0.1")
	add(a2, "198.51.100.0/24", "10.99.0.3")
	same("a second path for the other prefix", "192.0.2.0/24>10.99.0.3,198.51.100.0/24>10.99.0.1+10.99.0.3")
	a2.Stop()
	same("peer down", "198.51.100.0/24>10.99.0.1")
	del(a1, "198.51.100.0/24", "10.99.0.1")
	same("everything withdrawn", "")
}
