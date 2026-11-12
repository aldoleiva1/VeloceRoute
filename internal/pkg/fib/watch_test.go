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
	if got := f.table[netip.MustParsePrefix("192.0.2.0/24")]; got != netip.MustParseAddr("10.99.0.1") {
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
