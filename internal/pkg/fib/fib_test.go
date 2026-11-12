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
	"testing"

	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
)

// fakeProg records calls and keeps the table it would have programmed.
type fakeProg struct {
	table   map[netip.Prefix]netip.Addr
	calls   []string
	replace map[netip.Prefix]error // injected Replace errors
	delErr  map[netip.Prefix]error // injected Delete errors
	owned   []Route                // returned by ListOwned
}

func newFake() *fakeProg {
	return &fakeProg{table: map[netip.Prefix]netip.Addr{}, replace: map[netip.Prefix]error{}, delErr: map[netip.Prefix]error{}}
}

func (f *fakeProg) Replace(r Route) error {
	f.calls = append(f.calls, "replace "+r.String())
	if err := f.replace[r.Prefix]; err != nil {
		return err
	}
	f.table[r.Prefix] = r.NextHop
	return nil
}

func (f *fakeProg) Delete(p netip.Prefix) error {
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
	var s []string
	for p, nh := range f.table {
		s = append(s, fmt.Sprintf("%s>%s", p, nh))
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
	if got := f.table[netip.MustParsePrefix("2001:db8:2::/48")]; got != netip.MustParseAddr("fe80::1") {
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
	f.table[a], f.table[b] = netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("10.0.0.2")
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
