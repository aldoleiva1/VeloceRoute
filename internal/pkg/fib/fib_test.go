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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
)

// fakeProg records calls and keeps the table it would have programmed.
type fakeProg struct {
	mu      sync.Mutex // the real-server tests read the table while the watcher writes it
	table   map[netip.Prefix][]netip.Addr
	calls   []string
	replace map[netip.Prefix]error // injected Replace errors
	hook    func(Route) error      // injected Replace errors by route (checked first)
	delErr  map[netip.Prefix]error // injected Delete errors
	owned   []Route                // returned by ListOwned
}

func newFake() *fakeProg {
	return &fakeProg{table: map[netip.Prefix][]netip.Addr{}, replace: map[netip.Prefix]error{}, delErr: map[netip.Prefix]error{}}
}

func (f *fakeProg) Replace(r Route) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "replace "+r.String())
	if f.hook != nil {
		if err := f.hook(r); err != nil {
			return err
		}
	}
	if err := f.replace[r.Prefix]; err != nil {
		return err
	}
	f.table[r.Prefix] = r.NextHops
	return nil
}

func (f *fakeProg) Delete(p netip.Prefix) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "delete "+p.String())
	if err := f.delErr[p]; err != nil {
		return err
	}
	delete(f.table, p)
	return nil
}

func (f *fakeProg) ListOwned() ([]Route, error) { return f.owned, nil }

func (f *fakeProg) callLog() string { return strings.Join(f.calls, "; ") }

func (f *fakeProg) routes() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.routesLocked()
}

func (f *fakeProg) routesLocked() string {
	var s []string
	for p, nhs := range f.table {
		var h []string
		for _, a := range nhs {
			h = append(h, a.String())
		}
		s = append(s, fmt.Sprintf("%s>%s", p, strings.Join(h, "+")))
	}
	sort.Strings(s)
	return strings.Join(s, ",")
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

var peer = netip.MustParseAddr("10.0.0.2")

func v4(t testing.TB, prefix, nh string) *apiutil.Path {
	t.Helper()
	n, err := bgp.NewIPAddrPrefix(netip.MustParsePrefix(prefix))
	if err != nil {
		t.Fatal(err)
	}
	a, err := bgp.NewPathAttributeNextHop(netip.MustParseAddr(nh))
	if err != nil {
		t.Fatal(err)
	}
	return &apiutil.Path{Family: bgp.RF_IPv4_UC, Nlri: n, Attrs: []bgp.PathAttributeInterface{a}, PeerAddress: peer}
}

func v6(t testing.TB, prefix, nh string) *apiutil.Path {
	t.Helper()
	n, err := bgp.NewIPAddrPrefix(netip.MustParsePrefix(prefix))
	if err != nil {
		t.Fatal(err)
	}
	mp, err := bgp.NewPathAttributeMpReachNLRI(bgp.RF_IPv6_UC, []bgp.PathNLRI{{NLRI: n}}, netip.MustParseAddr(nh))
	if err != nil {
		t.Fatal(err)
	}
	return &apiutil.Path{Family: bgp.RF_IPv6_UC, Nlri: n, Attrs: []bgp.PathAttributeInterface{mp}, PeerAddress: peer}
}

func withdrawn(p *apiutil.Path) *apiutil.Path { c := *p; c.Withdrawal = true; return &c }

func TestInstallReplaceAndWithdraw(t *testing.T) {
	f := newFake()
	s := NewSyncer(f, quiet())

	s.HandleBestPaths([]*apiutil.Path{v4(t, "192.0.2.0/24", "10.0.0.2")})
	if got := f.routes(); got != "192.0.2.0/24>10.0.0.2" {
		t.Fatalf("after install: %s", got)
	}
	// The same best path again must not touch the kernel.
	s.HandleBestPaths([]*apiutil.Path{v4(t, "192.0.2.0/24", "10.0.0.2")})
	if n := len(f.calls); n != 1 {
		t.Fatalf("repeat caused kernel calls: %s", f.callLog())
	}
	// A new best path with another next hop replaces the route.
	s.HandleBestPaths([]*apiutil.Path{v4(t, "192.0.2.0/24", "10.0.0.9")})
	if got := f.routes(); got != "192.0.2.0/24>10.0.0.9" {
		t.Fatalf("after replace: %s", got)
	}
	s.HandleBestPaths([]*apiutil.Path{withdrawn(v4(t, "192.0.2.0/24", "10.0.0.9"))})
	if got := f.routes(); got != "" {
		t.Fatalf("after withdraw: %s", got)
	}
	if st := s.Stats(); st.Installed != 0 || st.Replaced != 2 || st.Deleted != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestWithdrawOfUnknownPrefixDoesNothing(t *testing.T) {
	f := newFake()
	s := NewSyncer(f, quiet())
	s.HandleBestPaths([]*apiutil.Path{withdrawn(v4(t, "198.51.100.0/24", "10.0.0.2"))})
	if len(f.calls) != 0 {
		t.Fatalf("unexpected kernel calls: %s", f.callLog())
	}
}

func TestIPv6AndLinkLocalNextHop(t *testing.T) {
	f := newFake()
	s := NewSyncer(f, quiet())
	s.HandleBestPaths([]*apiutil.Path{v6(t, "2001:db8:1::/48", "2001:db8::2")})
	if got := f.routes(); got != "2001:db8:1::/48>2001:db8::2" {
		t.Fatalf("v6: %s", got)
	}
	// A peer with only a link-local next hop.
	p := v6(t, "2001:db8:2::/48", "::")
	p.Attrs[0].(*bgp.PathAttributeMpReachNLRI).LinkLocalNexthop = netip.MustParseAddr("fe80::1")
	s.HandleBestPaths([]*apiutil.Path{p})
	if got := f.table[netip.MustParsePrefix("2001:db8:2::/48")]; len(got) != 1 || got[0] != netip.MustParseAddr("fe80::1") {
		t.Fatalf("link-local next hop: %v", got)
	}
}

func TestLocallyOriginatedPathIsNotProgrammed(t *testing.T) {
	f := newFake()
	s := NewSyncer(f, quiet())
	local := v4(t, "203.0.113.0/24", "0.0.0.0")
	local.PeerAddress = netip.Addr{} // no peer: we originated it
	s.HandleBestPaths([]*apiutil.Path{local})
	if len(f.calls) != 0 || s.Stats().Skipped != 1 {
		t.Fatalf("local path was programmed: %s stats=%+v", f.callLog(), s.Stats())
	}
}

// If a prefix we installed from a peer later has a locally originated best
// path, the old kernel route is stale and must go.
func TestLocalBestPathRemovesEarlierRoute(t *testing.T) {
	f := newFake()
	s := NewSyncer(f, quiet())
	s.HandleBestPaths([]*apiutil.Path{v4(t, "203.0.113.0/24", "10.0.0.2")})
	local := v4(t, "203.0.113.0/24", "10.0.0.1")
	local.PeerAddress = netip.Addr{}
	s.HandleBestPaths([]*apiutil.Path{local})
	if got := f.routes(); got != "" {
		t.Fatalf("stale route kept: %s", got)
	}
}

func TestIneligiblePathsAreSkipped(t *testing.T) {
	cases := map[string]*apiutil.Path{}

	// IPv4 prefix with an IPv6 next hop
	mismatch := v4(t, "192.0.2.0/24", "10.0.0.2")
	mismatch.Attrs = v6(t, "2001:db8::/48", "2001:db8::2").Attrs
	cases["next hop of the wrong family"] = mismatch

	invalid := v4(t, "192.0.2.0/24", "10.0.0.2")
	invalid.IsNexthopInvalid = true
	cases["next hop marked invalid"] = invalid

	none := v4(t, "192.0.2.0/24", "10.0.0.2")
	none.Attrs = nil
	cases["no next hop"] = none

	unspec := v4(t, "192.0.2.0/24", "0.0.0.0")
	cases["unspecified next hop"] = unspec

	vpn := v4(t, "192.0.2.0/24", "10.0.0.2")
	vpn.Family = bgp.RF_IPv4_VPN
	cases["not unicast"] = vpn

	nonPrefix := v4(t, "192.0.2.0/24", "10.0.0.2")
	nonPrefix.Nlri = &bgp.LabeledIPAddrPrefix{}
	cases["not a plain prefix"] = nonPrefix

	for name, p := range cases {
		f := newFake()
		s := NewSyncer(f, quiet())
		s.HandleBestPaths([]*apiutil.Path{p})
		if len(f.table) != 0 {
			t.Errorf("%s: programmed %s", name, f.routes())
		}
	}
	s := NewSyncer(newFake(), quiet())
	s.HandleBestPaths([]*apiutil.Path{nil})
}

func TestUnreachableNextHopIsSkippedThenRecovers(t *testing.T) {
	f := newFake()
	s := NewSyncer(f, quiet())
	pfx := netip.MustParsePrefix("192.0.2.0/24")
	f.replace[pfx] = fmt.Errorf("%w: network is unreachable", ErrNextHopUnreachable)

	s.HandleBestPaths([]*apiutil.Path{v4(t, "192.0.2.0/24", "172.16.0.9")})
	if st := s.Stats(); st.Installed != 0 || st.Unreachable != 1 {
		t.Fatalf("stats %+v", st)
	}
	// Withdrawing a route that never went in must not call Delete.
	s.HandleBestPaths([]*apiutil.Path{withdrawn(v4(t, "192.0.2.0/24", "172.16.0.9"))})
	for _, c := range f.calls {
		if strings.HasPrefix(c, "delete") {
			t.Fatalf("deleted a route that was never installed: %s", f.callLog())
		}
	}
	// Once the next hop becomes reachable the route is installed.
	delete(f.replace, pfx)
	s.HandleBestPaths([]*apiutil.Path{v4(t, "192.0.2.0/24", "10.0.0.2")})
	if got := f.routes(); got != "192.0.2.0/24>10.0.0.2" {
		t.Fatalf("did not recover: %s", got)
	}
}

// When the new best path cannot be installed, the old route (pointing at a
// path that is no longer best) is removed instead of left forwarding.
func TestFailedReplaceRemovesOldRoute(t *testing.T) {
	f := newFake()
	s := NewSyncer(f, quiet())
	pfx := netip.MustParsePrefix("192.0.2.0/24")
	s.HandleBestPaths([]*apiutil.Path{v4(t, "192.0.2.0/24", "10.0.0.2")})
	f.replace[pfx] = errors.New("boom")
	s.HandleBestPaths([]*apiutil.Path{v4(t, "192.0.2.0/24", "10.0.0.9")})
	if got := f.routes(); got != "" {
		t.Fatalf("stale route kept after failed replace: %s", got)
	}
	if s.Stats().Errors != 1 {
		t.Fatalf("stats %+v", s.Stats())
	}
}

func TestFailedDeleteIsRetried(t *testing.T) {
	f := newFake()
	s := NewSyncer(f, quiet())
	pfx := netip.MustParsePrefix("192.0.2.0/24")
	s.HandleBestPaths([]*apiutil.Path{v4(t, "192.0.2.0/24", "10.0.0.2")})
	f.delErr[pfx] = errors.New("busy")
	s.HandleBestPaths([]*apiutil.Path{withdrawn(v4(t, "192.0.2.0/24", "10.0.0.2"))})
	if s.Stats().Installed != 1 {
		t.Fatal("a route whose delete failed must stay tracked so it can be removed later")
	}
	delete(f.delErr, pfx)
	if err := s.RemoveAll(); err != nil || f.routes() != "" {
		t.Fatalf("RemoveAll: err=%v routes=%s", err, f.routes())
	}
}

func TestReconcileRemovesStaleRoutes(t *testing.T) {
	f := newFake()
	a, b := netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.51.100.0/24")
	f.table[a], f.table[b] = []netip.Addr{netip.MustParseAddr("10.0.0.2")}, []netip.Addr{netip.MustParseAddr("10.0.0.2")}
	f.owned = []Route{{Prefix: a}, {Prefix: b}}
	s := NewSyncer(f, quiet())
	if err := s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if f.routes() != "" {
		t.Fatalf("stale routes left: %s", f.routes())
	}
}

func TestRemoveAllContinuesAfterError(t *testing.T) {
	f := newFake()
	s := NewSyncer(f, quiet())
	for _, p := range []string{"192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24"} {
		s.HandleBestPaths([]*apiutil.Path{v4(t, p, "10.0.0.2")})
	}
	f.delErr[netip.MustParsePrefix("198.51.100.0/24")] = errors.New("busy")
	err := s.RemoveAll()
	if err == nil {
		t.Fatal("expected the delete error to be returned")
	}
	if got := f.routes(); got != "198.51.100.0/24>10.0.0.2" {
		t.Fatalf("other routes must still be removed, left: %s", got)
	}
}

func TestSeveralPathsInOneEvent(t *testing.T) {
	f := newFake()
	s := NewSyncer(f, quiet())
	s.HandleBestPaths([]*apiutil.Path{
		v4(t, "192.0.2.0/24", "10.0.0.2"),
		v6(t, "2001:db8:1::/48", "2001:db8::2"),
		withdrawn(v4(t, "192.0.2.0/24", "10.0.0.2")),
		v4(t, "198.51.100.0/24", "10.0.0.2"),
	})
	if got := f.routes(); got != "198.51.100.0/24>10.0.0.2,2001:db8:1::/48>2001:db8::2" {
		t.Fatalf("routes: %s", got)
	}
}

// An event still in flight when shutdown starts must not re-install a route.
func TestEventsAfterRemoveAllAreIgnored(t *testing.T) {
	f := newFake()
	s := NewSyncer(f, quiet())
	s.HandleBestPaths([]*apiutil.Path{v4(t, "192.0.2.0/24", "10.0.0.2")})
	if err := s.RemoveAll(); err != nil {
		t.Fatal(err)
	}
	s.HandleBestPaths([]*apiutil.Path{v4(t, "198.51.100.0/24", "10.0.0.2")})
	if f.routes() != "" {
		t.Fatalf("route installed after RemoveAll: %s", f.routes())
	}
}

// ---- ECMP / multiple paths (task E4-3) ----

func from(p *apiutil.Path, peerAddr string) *apiutil.Path {
	c := *p
	c.PeerAddress = netip.MustParseAddr(peerAddr)
	return &c
}

func newMulti(f *fakeProg, max int) *Syncer { return NewSyncer(f, quiet(), WithMultipath(max)) }

func TestMultipathBuildsOneEcmpRoute(t *testing.T) {
	f := newFake()
	s := newMulti(f, 0)
	s.HandleBestPaths([]*apiutil.Path{
		from(v4(t, "192.0.2.0/24", "10.0.0.3"), "10.0.0.3"),
		from(v4(t, "192.0.2.0/24", "10.0.0.2"), "10.0.0.2"),
	})
	if got := f.routes(); got != "192.0.2.0/24>10.0.0.2+10.0.0.3" {
		t.Fatalf("route: %s (calls: %s)", got, f.callLog())
	}
	if st := s.Stats(); st.Installed != 1 {
		t.Fatalf("one prefix, one route: %+v", st)
	}
	before := len(f.calls)
	// The same paths again change nothing.
	s.HandleBestPaths([]*apiutil.Path{from(v4(t, "192.0.2.0/24", "10.0.0.2"), "10.0.0.2")})
	if len(f.calls) != before {
		t.Fatalf("repeat caused kernel calls: %s", f.callLog())
	}
}

func TestMultipathWithdrawOnePathKeepsTheOthers(t *testing.T) {
	f := newFake()
	s := newMulti(f, 0)
	a := from(v4(t, "192.0.2.0/24", "10.0.0.2"), "10.0.0.2")
	b := from(v4(t, "192.0.2.0/24", "10.0.0.3"), "10.0.0.3")
	c := from(v4(t, "192.0.2.0/24", "10.0.0.4"), "10.0.0.4")
	s.HandleBestPaths([]*apiutil.Path{a, b, c})
	s.HandleBestPaths([]*apiutil.Path{withdrawn(b)})
	if got := f.routes(); got != "192.0.2.0/24>10.0.0.2+10.0.0.4" {
		t.Fatalf("after one path left: %s", got)
	}
	s.HandleBestPaths([]*apiutil.Path{withdrawn(a)})
	if got := f.routes(); got != "192.0.2.0/24>10.0.0.4" {
		t.Fatalf("after two paths left: %s", got)
	}
	s.HandleBestPaths([]*apiutil.Path{withdrawn(c)})
	if got := f.routes(); got != "" {
		t.Fatalf("after all paths left: %s", got)
	}
	if st := s.Stats(); st.Installed != 0 {
		t.Fatalf("%+v", st)
	}
}

func TestMultipathChangedNextHopReplacesOnlyThatMember(t *testing.T) {
	f := newFake()
	s := newMulti(f, 0)
	s.HandleBestPaths([]*apiutil.Path{
		from(v4(t, "192.0.2.0/24", "10.0.0.2"), "10.0.0.2"),
		from(v4(t, "192.0.2.0/24", "10.0.0.3"), "10.0.0.3"),
	})
	// The peer at .3 now announces another next hop.
	s.HandleBestPaths([]*apiutil.Path{from(v4(t, "192.0.2.0/24", "10.0.0.9"), "10.0.0.3")})
	if got := f.routes(); got != "192.0.2.0/24>10.0.0.2+10.0.0.9" {
		t.Fatalf("route: %s", got)
	}
}

func TestMultipathSameNextHopFromTwoPeersIsOneHop(t *testing.T) {
	f := newFake()
	s := newMulti(f, 0)
	s.HandleBestPaths([]*apiutil.Path{
		from(v4(t, "192.0.2.0/24", "10.0.0.9"), "10.0.0.2"),
		from(v4(t, "192.0.2.0/24", "10.0.0.9"), "10.0.0.3"),
	})
	if got := f.routes(); got != "192.0.2.0/24>10.0.0.9" {
		t.Fatalf("route: %s", got)
	}
	n := len(f.calls)
	// One of the two peers goes away; the other still gives that next hop, so
	// the kernel route is unchanged.
	s.HandleBestPaths([]*apiutil.Path{withdrawn(from(v4(t, "192.0.2.0/24", "10.0.0.9"), "10.0.0.2"))})
	if len(f.calls) != n || f.routes() != "192.0.2.0/24>10.0.0.9" {
		t.Fatalf("unneeded kernel call: %s", f.callLog())
	}
}

func TestMultipathAddPathIdsFromOnePeerAreSeparatePaths(t *testing.T) {
	f := newFake()
	s := newMulti(f, 0)
	p1 := v4(t, "192.0.2.0/24", "10.0.0.8")
	p1.RemoteID = 1
	p2 := v4(t, "192.0.2.0/24", "10.0.0.9")
	p2.RemoteID = 2
	s.HandleBestPaths([]*apiutil.Path{p1, p2})
	if got := f.routes(); got != "192.0.2.0/24>10.0.0.8+10.0.0.9" {
		t.Fatalf("route: %s", got)
	}
	s.HandleBestPaths([]*apiutil.Path{withdrawn(p1)})
	if got := f.routes(); got != "192.0.2.0/24>10.0.0.9" {
		t.Fatalf("route after withdrawing path id 1: %s", got)
	}
}

func TestMultipathIneligiblePathRemovesOnlyItself(t *testing.T) {
	f := newFake()
	s := newMulti(f, 0)
	a := from(v4(t, "192.0.2.0/24", "10.0.0.2"), "10.0.0.2")
	b := from(v4(t, "192.0.2.0/24", "10.0.0.3"), "10.0.0.3")
	s.HandleBestPaths([]*apiutil.Path{a, b})
	bad := *b
	bad.IsNexthopInvalid = true
	s.HandleBestPaths([]*apiutil.Path{&bad})
	if got := f.routes(); got != "192.0.2.0/24>10.0.0.2" {
		t.Fatalf("route: %s", got)
	}
	if s.Stats().Skipped != 1 {
		t.Fatalf("%+v", s.Stats())
	}
}

func TestMultipathLimitKeepsLowestAddresses(t *testing.T) {
	f := newFake()
	s := newMulti(f, 2)
	for _, h := range []string{"10.0.0.5", "10.0.0.3", "10.0.0.4"} {
		s.HandleBestPaths([]*apiutil.Path{from(v4(t, "192.0.2.0/24", h), h)})
	}
	if got := f.routes(); got != "192.0.2.0/24>10.0.0.3+10.0.0.4" {
		t.Fatalf("route: %s", got)
	}
}

// Without WithMultipath the events are best paths: a new best path from a
// different peer replaces the old one, it does not join it.
func TestSinglePathModeReplacesAcrossPeers(t *testing.T) {
	f := newFake()
	s := NewSyncer(f, quiet())
	s.HandleBestPaths([]*apiutil.Path{from(v4(t, "192.0.2.0/24", "10.0.0.2"), "10.0.0.2")})
	s.HandleBestPaths([]*apiutil.Path{from(v4(t, "192.0.2.0/24", "10.0.0.3"), "10.0.0.3")})
	if got := f.routes(); got != "192.0.2.0/24>10.0.0.3" {
		t.Fatalf("route: %s", got)
	}
	s.HandleBestPaths([]*apiutil.Path{withdrawn(from(v4(t, "192.0.2.0/24", "10.0.0.3"), "10.0.0.3"))})
	if got := f.routes(); got != "" {
		t.Fatalf("stale route kept: %s", got)
	}
}

func unreachableHops(bad ...string) func(Route) error {
	return func(r Route) error {
		for _, h := range r.NextHops {
			for _, b := range bad {
				if h == netip.MustParseAddr(b) {
					return fmt.Errorf("%w: network is unreachable", ErrNextHopUnreachable)
				}
			}
		}
		return nil
	}
}

// One unreachable next hop must not take the reachable ones down with it.
func TestMultipathInstallsReachableNextHopsOnly(t *testing.T) {
	f := newFake()
	f.hook = unreachableHops("172.31.255.1")
	s := newMulti(f, 0)
	s.HandleBestPaths([]*apiutil.Path{
		from(v4(t, "192.0.2.0/24", "10.0.0.2"), "10.0.0.2"),
		from(v4(t, "192.0.2.0/24", "172.31.255.1"), "10.0.0.3"),
		from(v4(t, "192.0.2.0/24", "10.0.0.4"), "10.0.0.4"),
	})
	if got := f.routes(); got != "192.0.2.0/24>10.0.0.2+10.0.0.4" {
		t.Fatalf("route: %s (calls: %s)", got, f.callLog())
	}
	if st := s.Stats(); st.Installed != 1 || st.Unreachable != 1 || st.Errors != 0 || st.Replaced != 1 {
		t.Fatalf("%+v", st)
	}
	// Repeating the event must not probe the kernel again.
	n := len(f.calls)
	s.HandleBestPaths([]*apiutil.Path{from(v4(t, "192.0.2.0/24", "10.0.0.2"), "10.0.0.2")})
	if len(f.calls) != n {
		t.Fatalf("repeat probed the kernel again: %s", f.callLog())
	}
}

func TestMultipathAllNextHopsUnreachableRemovesOldRoute(t *testing.T) {
	f := newFake()
	s := newMulti(f, 0)
	s.HandleBestPaths([]*apiutil.Path{from(v4(t, "192.0.2.0/24", "10.0.0.2"), "10.0.0.2")})
	f.hook = unreachableHops("172.31.255.1", "172.31.255.2")
	s.HandleBestPaths([]*apiutil.Path{withdrawn(from(v4(t, "192.0.2.0/24", "10.0.0.2"), "10.0.0.2"))})
	s.HandleBestPaths([]*apiutil.Path{
		from(v4(t, "192.0.2.0/24", "172.31.255.1"), "10.0.0.3"),
		from(v4(t, "192.0.2.0/24", "172.31.255.2"), "10.0.0.4"),
	})
	if got := f.routes(); got != "" {
		t.Fatalf("route installed: %s", got)
	}
	if s.Stats().Installed != 0 {
		t.Fatalf("%+v", s.Stats())
	}
}

// A failure that is not "unreachable" while probing or on the final install
// must not leave the single-next-hop probe route in the kernel.
func TestMultipathFailureAfterProbesLeavesNothing(t *testing.T) {
	f := newFake()
	f.hook = func(r Route) error {
		for _, h := range r.NextHops {
			if h == netip.MustParseAddr("172.31.255.1") {
				return fmt.Errorf("%w: network is unreachable", ErrNextHopUnreachable)
			}
		}
		if len(r.NextHops) > 1 {
			return errors.New("boom")
		}
		return nil
	}
	s := newMulti(f, 0)
	s.HandleBestPaths([]*apiutil.Path{
		from(v4(t, "192.0.2.0/24", "10.0.0.2"), "10.0.0.2"),
		from(v4(t, "192.0.2.0/24", "10.0.0.4"), "10.0.0.4"),
		from(v4(t, "192.0.2.0/24", "172.31.255.1"), "10.0.0.3"),
	})
	if got := f.routes(); got != "" {
		t.Fatalf("probe route left behind: %s (calls: %s)", got, f.callLog())
	}
	if s.Stats().Installed != 0 || s.Stats().Errors != 1 {
		t.Fatalf("%+v", s.Stats())
	}
}

func TestMultipathRemoveAllDeletesEachPrefixOnce(t *testing.T) {
	f := newFake()
	s := newMulti(f, 0)
	s.HandleBestPaths([]*apiutil.Path{
		from(v4(t, "192.0.2.0/24", "10.0.0.2"), "10.0.0.2"),
		from(v4(t, "192.0.2.0/24", "10.0.0.3"), "10.0.0.3"),
	})
	f.calls = nil
	if err := s.RemoveAll(); err != nil || f.routes() != "" {
		t.Fatalf("RemoveAll: %v %s", err, f.routes())
	}
	if f.callLog() != "delete 192.0.2.0/24" {
		t.Fatalf("calls: %s", f.callLog())
	}
}

func TestReconcileDeletesAPrefixListedPerNextHopOnce(t *testing.T) {
	f := newFake()
	pf := netip.MustParsePrefix("192.0.2.0/24")
	f.table[pf] = []netip.Addr{netip.MustParseAddr("10.0.0.2")}
	f.owned = []Route{{Prefix: pf, NextHops: []netip.Addr{netip.MustParseAddr("10.0.0.2")}}, {Prefix: pf, NextHops: []netip.Addr{netip.MustParseAddr("10.0.0.3")}}}
	if err := NewSyncer(f, quiet()).Reconcile(); err != nil {
		t.Fatal(err)
	}
	if f.callLog() != "delete 192.0.2.0/24" {
		t.Fatalf("calls: %s", f.callLog())
	}
}
