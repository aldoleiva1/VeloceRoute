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

package server

import (
	"context"
	"fmt"
	"math/rand"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	api "github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/internal/pkg/table"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
)

func startLocalServer(t testing.TB) *BgpServer {
	t.Helper()
	s := NewBgpServer()
	go s.Serve()
	require.NoError(t, s.StartBgp(context.Background(), &api.StartBgpRequest{
		Global: &api.Global{Asn: 1, RouterId: "1.1.1.1", ListenPort: -1},
	}))
	t.Cleanup(func() { s.StopBgp(context.Background(), &api.StopBgpRequest{}) })
	return s
}

func localAPIPath(t testing.TB, prefix, nexthop string) *apiutil.Path {
	t.Helper()
	pfx := netip.MustParsePrefix(prefix)
	nh, err := bgp.NewPathAttributeNextHop(netip.MustParseAddr(nexthop))
	require.NoError(t, err)
	nlri, err := bgp.NewIPAddrPrefix(pfx)
	require.NoError(t, err)
	family := bgp.RF_IPv4_UC
	if pfx.Addr().Is6() {
		family = bgp.RF_IPv6_UC
	}
	path, err := apiutil.NewPath(family, nlri, false, []bgp.PathAttributeInterface{bgp.NewPathAttributeOrigin(0), nh}, time.Now())
	require.NoError(t, err)
	return mustApi2apiutilPath(path)
}

func nextChange(t testing.TB, w *watcher) *watchEventBestPathChange {
	t.Helper()
	select {
	case ev := <-w.Event():
		return ev.(*watchEventBestPathChange)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a best path change event")
		return nil
	}
}

func nextBest(t testing.TB, w *watcher) *watchEventBestPath {
	t.Helper()
	select {
	case ev := <-w.Event():
		return ev.(*watchEventBestPath)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a best path event")
		return nil
	}
}

func TestWatchBestPathChangesReportsAddNexthopChangeAndWithdraw(t *testing.T) {
	s := startLocalServer(t)
	w, err := s.watch(WatchBestPathChanges(false))
	require.NoError(t, err)
	defer w.Stop()

	_, err = s.AddPath(apiutil.AddPathRequest{Paths: []*apiutil.Path{localAPIPath(t, "192.0.2.0/24", "10.0.0.1")}})
	require.NoError(t, err)
	ev := nextChange(t, w)
	require.Len(t, ev.Changes, 1)
	c := ev.Changes[0]
	assert.Equal(t, bgp.RF_IPv4_UC, c.Family)
	assert.Equal(t, "192.0.2.0/24", c.Nlri.String())
	assert.Equal(t, netip.MustParseAddr("10.0.0.1"), c.NextHop)
	assert.False(t, c.Withdrawal)
	assert.False(t, c.PeerAddress.IsValid(), "a local path has no peer")

	// Same prefix, other next hop.
	_, err = s.AddPath(apiutil.AddPathRequest{Paths: []*apiutil.Path{localAPIPath(t, "192.0.2.0/24", "10.0.0.2")}})
	require.NoError(t, err)
	ev = nextChange(t, w)
	require.Len(t, ev.Changes, 1)
	assert.Equal(t, netip.MustParseAddr("10.0.0.2"), ev.Changes[0].NextHop)

	require.NoError(t, s.DeletePath(apiutil.DeletePathRequest{Paths: []*apiutil.Path{localAPIPath(t, "192.0.2.0/24", "10.0.0.2")}}))
	ev = nextChange(t, w)
	require.Len(t, ev.Changes, 1)
	assert.True(t, ev.Changes[0].Withdrawal)
	assert.Equal(t, "192.0.2.0/24", ev.Changes[0].Nlri.String())
}

func TestWatchBestPathChangesCurrentDeliversTheSnapshotFirst(t *testing.T) {
	s := startLocalServer(t)
	for _, p := range []string{"192.0.2.0/24", "198.51.100.0/24", "2001:db8::/32"} {
		nh := "10.0.0.1"
		if netip.MustParsePrefix(p).Addr().Is6() {
			nh = "2001:db8::1"
		}
		_, err := s.AddPath(apiutil.AddPathRequest{Paths: []*apiutil.Path{localAPIPath(t, p, nh)}})
		require.NoError(t, err)
	}
	w, err := s.watch(WatchBestPathChanges(true))
	require.NoError(t, err)
	defer w.Stop()

	ev := nextChange(t, w)
	got := map[string]string{}
	for _, c := range ev.Changes {
		got[c.Nlri.String()] = c.NextHop.String()
	}
	assert.Equal(t, map[string]string{
		"192.0.2.0/24": "10.0.0.1", "198.51.100.0/24": "10.0.0.1", "2001:db8::/32": "2001:db8::1",
	}, got)

	// Without current there is no snapshot.
	w2, err := s.watch(WatchBestPathChanges(false))
	require.NoError(t, err)
	defer w2.Stop()
	select {
	case ev := <-w2.Event():
		t.Fatalf("unexpected event without current: %+v", ev)
	case <-time.After(200 * time.Millisecond):
	}
}

// The compact event must say what the full one does. A random run of adds,
// replacements and withdrawals goes to both kinds of watcher at once, and
// every event is compared.
func TestBestPathChangesMatchTheFullEvents(t *testing.T) {
	s := startLocalServer(t)
	full, err := s.watch(WatchBestPath(false))
	require.NoError(t, err)
	defer full.Stop()
	compact, err := s.watch(WatchBestPathChanges(false))
	require.NoError(t, err)
	defer compact.Stop()

	prefixes := []string{"10.1.0.0/16", "10.2.0.0/16", "10.3.0.0/24", "172.16.0.0/12", "2001:db8:1::/48", "2001:db8:2::/48"}
	hops := map[bool][]string{false: {"192.0.2.1", "192.0.2.2", "192.0.2.3"}, true: {"2001:db8::1", "2001:db8::2"}}
	present := map[string]string{}
	rnd := rand.New(rand.NewSource(7))
	for i := 0; i < 300; i++ {
		p := prefixes[rnd.Intn(len(prefixes))]
		v6 := netip.MustParsePrefix(p).Addr().Is6()
		if nh, ok := present[p]; ok && rnd.Intn(3) == 0 {
			require.NoError(t, s.DeletePath(apiutil.DeletePathRequest{Paths: []*apiutil.Path{localAPIPath(t, p, nh)}}))
			delete(present, p)
		} else {
			nh := hops[v6][rnd.Intn(len(hops[v6]))]
			_, err := s.AddPath(apiutil.AddPathRequest{Paths: []*apiutil.Path{localAPIPath(t, p, nh)}})
			require.NoError(t, err)
			present[p] = nh
		}
		fe, ce := nextBest(t, full), nextChange(t, compact)
		paths := locRIBPathsForBMP(fe)
		require.Len(t, ce.Changes, len(paths), "step %d", i)
		for j, fp := range paths {
			c := ce.Changes[j]
			msg := fmt.Sprintf("step %d path %d", i, j)
			assert.Equal(t, fp.GetFamily(), c.Family, msg)
			assert.Equal(t, fp.GetNlri().String(), c.Nlri.String(), msg)
			assert.Equal(t, fp.IsWithdraw, c.Withdrawal, msg)
			assert.Equal(t, fp.IsNexthopInvalid, c.IsNexthopInvalid, msg)
			assert.Equal(t, fp.RemoteID(), c.RemoteID, msg)
			if !fp.IsWithdraw {
				assert.Equal(t, fp.GetNexthop(), c.NextHop, msg)
			}
			if src := fp.GetSource(); src != nil {
				assert.Equal(t, src.Address, c.PeerAddress, msg)
			}
		}
	}
}

// With only compact watchers the BGP loop does not clone paths, build the
// vrf map or keep anything; it makes the one event and its change slice.
func TestCompactWatcherAloneDoesNotClonePaths(t *testing.T) {
	s := startLocalServer(t)
	w, err := s.watch(WatchBestPathChanges(false))
	require.NoError(t, err)
	defer w.Stop()
	go func() {
		for range w.Event() {
		}
	}()
	_, err = s.AddPath(apiutil.AddPathRequest{Paths: []*apiutil.Path{localAPIPath(t, "192.0.2.0/24", "10.0.0.1")}})
	require.NoError(t, err)
	paths := s.globalRib.GetBestPathList("global", 0, nil)
	require.Len(t, paths, 1)

	compact := testing.AllocsPerRun(200, func() { s.notifyBestWatcher(paths, nil, paths, nil) })
	// One event, one slice of changes; the channel hand-over adds a little.
	assert.LessOrEqual(t, compact, 4.0)

	full, err := s.watch(WatchBestPath(false))
	require.NoError(t, err)
	defer full.Stop()
	go func() {
		for range full.Event() {
		}
	}()
	both := testing.AllocsPerRun(200, func() { s.notifyBestWatcher(paths, nil, paths, nil) })
	assert.Greater(t, both, compact+4, "the full watcher should cost the clones")
}

func BenchmarkNotifyBestWatcher(b *testing.B) {
	for _, kind := range []string{"none", "compact", "full"} {
		b.Run(kind, func(b *testing.B) {
			s := startLocalServer(b)
			var w *watcher
			var err error
			switch kind {
			case "compact":
				w, err = s.watch(WatchBestPathChanges(false))
			case "full":
				w, err = s.watch(WatchBestPath(false))
			}
			require.NoError(b, err)
			if w != nil {
				defer w.Stop()
				go func() {
					for range w.Event() {
					}
				}()
			}
			_, err = s.AddPath(apiutil.AddPathRequest{Paths: []*apiutil.Path{localAPIPath(b, "192.0.2.0/24", "10.0.0.1")}})
			require.NoError(b, err)
			paths := s.globalRib.GetBestPathList("global", 0, nil)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s.notifyBestWatcher(paths, nil, paths, nil)
			}
		})
	}
}

// Which paths a change event reports, and in which order, is part of the
// contract: for one prefix a withdrawal followed by an update means "replaced",
// the other way round means "gone".
func TestBestPathChangeEventSelection(t *testing.T) {
	src := &table.PeerInfo{AS: 65000, LocalAS: 65001, ID: netip.MustParseAddr("10.0.0.2"), Address: netip.MustParseAddr("10.0.0.2")}
	mk := func(prefix string, withdraw bool) *table.Path {
		nlri, _ := bgp.NewIPAddrPrefix(netip.MustParsePrefix(prefix))
		nh, _ := bgp.NewPathAttributeNextHop(netip.MustParseAddr("10.0.0.2"))
		return table.NewPath(bgp.RF_IPv4_UC, src, bgp.PathNLRI{NLRI: nlri}, withdraw, []bgp.PathAttributeInterface{bgp.NewPathAttributeOrigin(0), nh}, time.Now(), false)
	}
	desc := func(ev *watchEventBestPathChange) string {
		var s []string
		for _, c := range ev.Changes {
			w := "+"
			if c.Withdrawal {
				w = "-"
			}
			s = append(s, w+c.Nlri.String())
		}
		return fmt.Sprint(s)
	}
	a, b, c := mk("10.1.0.0/24", false), mk("10.2.0.0/24", false), mk("10.3.0.0/24", false)
	aw := mk("10.1.0.0/24", true)

	// Deltas win over everything and the withdrawals come first.
	assert.Equal(t, "[-10.1.0.0/24 +10.1.0.0/24 +10.2.0.0/24]",
		desc(newBestPathChangeEvent([]*table.Path{c}, [][]*table.Path{{c}}, []*table.Path{a, b}, []*table.Path{aw})))
	// A withdrawal alone is still a delta.
	assert.Equal(t, "[-10.1.0.0/24]", desc(newBestPathChangeEvent([]*table.Path{c}, nil, nil, []*table.Path{aw})))
	// With no deltas, the multipaths, flattened in order.
	assert.Equal(t, "[+10.1.0.0/24 +10.2.0.0/24 +10.3.0.0/24]",
		desc(newBestPathChangeEvent([]*table.Path{c}, [][]*table.Path{{a, b}, {c}}, nil, nil)))
	// With neither, the best paths; nil entries are skipped.
	assert.Equal(t, "[+10.3.0.0/24 +10.2.0.0/24]", desc(newBestPathChangeEvent([]*table.Path{c, nil, b}, nil, nil, nil)))
	// And an empty notification is an empty event, not a panic.
	assert.Empty(t, newBestPathChangeEvent(nil, nil, nil, nil).Changes)

	// It agrees with the full event's own selection.
	full := &watchEventBestPath{PathList: []*table.Path{c}, MultiPathList: [][]*table.Path{{a, b}, {c}}, UpdatePathList: []*table.Path{a}, WithdrawPathList: []*table.Path{aw}}
	assert.Len(t, newBestPathChangeEvent(full.PathList, full.MultiPathList, full.UpdatePathList, full.WithdrawPathList).Changes, len(locRIBPathsForBMP(full)))
}
