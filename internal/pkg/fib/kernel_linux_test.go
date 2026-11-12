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
	"net"
	"net/netip"
	"runtime"
	"testing"

	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// These tests program a real kernel inside a throwaway network namespace, so
// they never touch the host's routes. They need CAP_NET_ADMIN and skip without
// it. Run them as root, or in a privileged container (see docs/veloceroute/fib.md).

type testNS struct {
	t  *testing.T
	ns netns.NsHandle
	h  *netlink.Handle
}

func newTestNS(t *testing.T) *testNS {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	orig, err := netns.Get()
	if err != nil {
		t.Skipf("cannot read the current network namespace: %v", err)
	}
	defer orig.Close()
	ns, err := netns.New() // creates the namespace and enters it
	if err != nil {
		t.Skipf("cannot create a network namespace (need CAP_NET_ADMIN): %v", err)
	}
	if err := netns.Set(orig); err != nil {
		t.Fatalf("leave the test namespace: %v", err)
	}
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		t.Fatal(err)
	}
	n := &testNS{t: t, ns: ns, h: h}
	t.Cleanup(func() { h.Close(); ns.Close() })

	// A veth pair gives the namespace a real on-link network: 10.0.12.0/24 and
	// 2001:db8:12::/64 are connected, so next hops inside them are reachable.
	if err := h.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "v0"}, PeerName: "v1"}); err != nil {
		t.Skipf("cannot create a veth pair: %v", err)
	}
	for _, name := range []string{"v0", "v1"} {
		l, err := h.LinkByName(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.LinkSetUp(l); err != nil {
			t.Fatal(err)
		}
	}
	v0, _ := h.LinkByName("v0")
	n.addr(v0, "10.0.12.1/24")
	return n
}

func (n *testNS) addr(l netlink.Link, cidr string) bool {
	a, err := netlink.ParseAddr(cidr)
	if err != nil {
		n.t.Fatal(err)
	}
	if err := n.h.AddrAdd(l, a); err != nil {
		n.t.Logf("address %s not added: %v", cidr, err)
		return false
	}
	return true
}

// routes lists every route in the main table for a family.
func (n *testNS) routes(family int) []netlink.Route {
	n.t.Helper()
	rs, err := n.h.RouteListFiltered(family, &netlink.Route{Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_TABLE)
	if err != nil {
		n.t.Fatal(err)
	}
	return rs
}

// find returns the routes for a destination (nil for the default route).
func (n *testNS) find(family int, dst string) []netlink.Route {
	var out []netlink.Route
	for _, r := range n.routes(family) {
		if dst == "" && r.Dst == nil || r.Dst != nil && r.Dst.String() == dst {
			out = append(out, r)
		}
	}
	return out
}

func (n *testNS) programmer(t *testing.T) interface {
	Programmer
	Close() error
} {
	t.Helper()
	p, err := NewKernelProgrammer(KernelOptions{NetNS: &n.ns})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

func pfx(s string) netip.Prefix { return netip.MustParsePrefix(s) }
func ip(s string) netip.Addr    { return netip.MustParseAddr(s) }

func TestKernelInstallReplaceDelete(t *testing.T) {
	n := newTestNS(t)
	p := n.programmer(t)

	if err := p.Replace(Route{pfx("192.0.2.0/24"), ip("10.0.12.2")}); err != nil {
		t.Fatalf("install: %v", err)
	}
	rs := n.find(netlink.FAMILY_V4, "192.0.2.0/24")
	if len(rs) != 1 {
		t.Fatalf("want one route, got %v", rs)
	}
	r := rs[0]
	if r.Protocol != DefaultProtocol || r.Priority != DefaultMetric || !r.Gw.Equal(net.ParseIP("10.0.12.2")) {
		t.Fatalf("route not stamped as ours: proto=%d metric=%d gw=%v", r.Protocol, r.Priority, r.Gw)
	}

	// Replacing with another next hop leaves exactly one route.
	if err := p.Replace(Route{pfx("192.0.2.0/24"), ip("10.0.12.3")}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	rs = n.find(netlink.FAMILY_V4, "192.0.2.0/24")
	if len(rs) != 1 || !rs[0].Gw.Equal(net.ParseIP("10.0.12.3")) {
		t.Fatalf("after replace: %v", rs)
	}

	owned, err := p.ListOwned()
	if err != nil || len(owned) != 1 || owned[0].Prefix != pfx("192.0.2.0/24") || owned[0].NextHop != ip("10.0.12.3") {
		t.Fatalf("ListOwned = %v, %v", owned, err)
	}

	if err := p.Delete(pfx("192.0.2.0/24")); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if rs := n.find(netlink.FAMILY_V4, "192.0.2.0/24"); len(rs) != 0 {
		t.Fatalf("route still there: %v", rs)
	}
	// Deleting what is not there is not an error.
	if err := p.Delete(pfx("192.0.2.0/24")); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

// The safety property: routes that are not ours are never replaced, deleted or
// listed, even for the very same prefix.
func TestKernelNeverTouchesForeignRoutes(t *testing.T) {
	n := newTestNS(t)
	p := n.programmer(t)

	// A static route for the prefix we are about to program (metric 0 by
	// default, protocol static) and the kernel's own connected 10.0.12.0/24.
	_, dst, _ := net.ParseCIDR("192.0.2.0/24")
	foreign := &netlink.Route{Dst: dst, Gw: net.ParseIP("10.0.12.9"), Protocol: unix.RTPROT_STATIC, Table: unix.RT_TABLE_MAIN}
	if err := n.h.RouteAdd(foreign); err != nil {
		t.Fatal(err)
	}
	connectedBefore := len(n.find(netlink.FAMILY_V4, "10.0.12.0/24"))
	if connectedBefore == 0 {
		t.Fatal("expected the kernel's connected route")
	}

	// Program the same prefix, and the connected prefix.
	if err := p.Replace(Route{pfx("192.0.2.0/24"), ip("10.0.12.2")}); err != nil {
		t.Fatal(err)
	}
	if err := p.Replace(Route{pfx("10.0.12.0/24"), ip("10.0.12.7")}); err != nil {
		t.Fatal(err)
	}

	rs := n.find(netlink.FAMILY_V4, "192.0.2.0/24")
	if len(rs) != 2 {
		t.Fatalf("our route and the static route must coexist, got %v", rs)
	}
	var static, ours int
	for _, r := range rs {
		switch r.Protocol {
		case unix.RTPROT_STATIC:
			static++
			if !r.Gw.Equal(net.ParseIP("10.0.12.9")) {
				t.Fatalf("the static route was modified: %v", r)
			}
		case DefaultProtocol:
			ours++
		}
	}
	if static != 1 || ours != 1 {
		t.Fatalf("static=%d ours=%d", static, ours)
	}
	var kernelConnected int
	for _, r := range n.find(netlink.FAMILY_V4, "10.0.12.0/24") {
		if r.Protocol == unix.RTPROT_KERNEL {
			kernelConnected++
		}
	}
	if kernelConnected != 1 {
		t.Fatalf("the kernel's connected route was replaced or removed (%d left)", kernelConnected)
	}

	// ListOwned sees only ours.
	owned, err := p.ListOwned()
	if err != nil || len(owned) != 2 {
		t.Fatalf("ListOwned = %v, %v", owned, err)
	}

	// Deleting ours leaves the static route and the connected route.
	for _, x := range []string{"192.0.2.0/24", "10.0.12.0/24"} {
		if err := p.Delete(pfx(x)); err != nil {
			t.Fatal(err)
		}
	}
	rs = n.find(netlink.FAMILY_V4, "192.0.2.0/24")
	if len(rs) != 1 || rs[0].Protocol != unix.RTPROT_STATIC {
		t.Fatalf("static route lost: %v", rs)
	}
	if got := len(n.find(netlink.FAMILY_V4, "10.0.12.0/24")); got != connectedBefore {
		t.Fatalf("connected route count %d, want %d", got, connectedBefore)
	}
}

func TestKernelUnreachableNextHop(t *testing.T) {
	n := newTestNS(t)
	p := n.programmer(t)
	err := p.Replace(Route{pfx("192.0.2.0/24"), ip("172.31.255.1")}) // no route to it
	if !errors.Is(err, ErrNextHopUnreachable) {
		t.Fatalf("err = %v, want ErrNextHopUnreachable", err)
	}
	if rs := n.find(netlink.FAMILY_V4, "192.0.2.0/24"); len(rs) != 0 {
		t.Fatalf("route installed anyway: %v", rs)
	}
}

func TestKernelDefaultRoute(t *testing.T) {
	n := newTestNS(t)
	p := n.programmer(t)
	if err := p.Replace(Route{pfx("0.0.0.0/0"), ip("10.0.12.2")}); err != nil {
		t.Fatalf("default route: %v", err)
	}
	owned, err := p.ListOwned()
	if err != nil || len(owned) != 1 || owned[0].Prefix != pfx("0.0.0.0/0") {
		t.Fatalf("ListOwned = %v, %v", owned, err)
	}
	if err := p.Delete(pfx("0.0.0.0/0")); err != nil {
		t.Fatal(err)
	}
	if owned, _ := p.ListOwned(); len(owned) != 0 {
		t.Fatalf("default route left: %v", owned)
	}
}

func TestKernelIPv6(t *testing.T) {
	n := newTestNS(t)
	v0, _ := n.h.LinkByName("v0")
	if !n.addr(v0, "2001:db8:12::1/64") {
		t.Skip("IPv6 is not available in this environment")
	}
	p := n.programmer(t)
	if err := p.Replace(Route{pfx("2001:db8:99::/48"), ip("2001:db8:12::2")}); err != nil {
		t.Fatalf("v6 install: %v", err)
	}
	owned, err := p.ListOwned()
	if err != nil || len(owned) != 1 || owned[0].Prefix != pfx("2001:db8:99::/48") || owned[0].NextHop != ip("2001:db8:12::2") {
		t.Fatalf("ListOwned = %v, %v", owned, err)
	}
	if err := p.Delete(pfx("2001:db8:99::/48")); err != nil {
		t.Fatal(err)
	}
	if owned, _ := p.ListOwned(); len(owned) != 0 {
		t.Fatalf("v6 route left: %v", owned)
	}
}

func TestKernelOptionsValidation(t *testing.T) {
	for name, o := range map[string]KernelOptions{
		"kernel protocol":  {Protocol: unix.RTPROT_KERNEL},
		"static protocol":  {Protocol: unix.RTPROT_STATIC},
		"protocol too big": {Protocol: 256},
		"local table":      {Table: unix.RT_TABLE_LOCAL},
		"negative metric":  {Metric: -1},
	} {
		if p, err := NewKernelProgrammer(o); err == nil {
			p.Close()
			t.Errorf("%s: expected an error", name)
		}
	}
}

// The whole chain on a real kernel: stale routes from an earlier run are
// removed first, learned best paths are installed and withdrawn, and
// RemoveAll leaves nothing behind.
func TestKernelSyncerEndToEnd(t *testing.T) {
	n := newTestNS(t)
	p := n.programmer(t)

	// A route our protocol left behind in an earlier run.
	if err := p.Replace(Route{pfx("198.51.100.0/24"), ip("10.0.12.2")}); err != nil {
		t.Fatal(err)
	}
	s := NewSyncer(p, quiet())
	if err := s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if rs := n.find(netlink.FAMILY_V4, "198.51.100.0/24"); len(rs) != 0 {
		t.Fatalf("stale route survived Reconcile: %v", rs)
	}

	s.HandleBestPaths([]*apiutil.Path{
		v4(t, "192.0.2.0/24", "10.0.12.2"),
		v4(t, "203.0.113.0/24", "172.31.255.1"), // unreachable: skipped, not fatal
		v4(t, "198.18.0.0/15", "10.0.12.3"),
	})
	if got := len(n.find(netlink.FAMILY_V4, "192.0.2.0/24")); got != 1 {
		t.Fatalf("192.0.2.0/24 not installed")
	}
	if got := len(n.find(netlink.FAMILY_V4, "203.0.113.0/24")); got != 0 {
		t.Fatalf("unreachable route was installed")
	}
	if st := s.Stats(); st.Installed != 2 || st.Unreachable != 1 {
		t.Fatalf("stats %+v", st)
	}

	s.HandleBestPaths([]*apiutil.Path{withdrawn(v4(t, "192.0.2.0/24", "10.0.12.2"))})
	if got := len(n.find(netlink.FAMILY_V4, "192.0.2.0/24")); got != 0 {
		t.Fatalf("withdrawn route still in the kernel")
	}

	if err := s.RemoveAll(); err != nil {
		t.Fatal(err)
	}
	if owned, _ := p.ListOwned(); len(owned) != 0 {
		t.Fatalf("routes left after RemoveAll: %v", owned)
	}
}

// canCreateNS reports whether a network namespace can be created. It restores
// the calling thread's namespace, which netns.New would otherwise leave changed.
func canCreateNS() bool {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	orig, err := netns.Get()
	if err != nil {
		return false
	}
	defer orig.Close()
	ns, err := netns.New()
	if err != nil {
		return false
	}
	defer ns.Close()
	_ = netns.Set(orig)
	return true
}

func TestCheckPrivilegesAgreesWithNamespaceCreation(t *testing.T) {
	err := CheckPrivileges()
	if canCreateNS() && err != nil {
		t.Fatalf("able to create a namespace but CheckPrivileges says: %v", err)
	}
	if !canCreateNS() && err == nil {
		t.Fatalf("cannot create a namespace but CheckPrivileges reports the capability")
	}
}

// The normal production path: no namespace given, so the handle is opened in
// the current namespace. This only reads (a dump of routes with our protocol),
// so it is safe anywhere. It guards against treating a zero-value namespace
// handle, which is file descriptor 0, as a real namespace.
func TestKernelProgrammerOpensInCurrentNamespace(t *testing.T) {
	p, err := NewKernelProgrammer(KernelOptions{})
	if err != nil {
		t.Fatalf("default options: %v", err)
	}
	defer p.Close()
	if _, err := p.ListOwned(); err != nil {
		t.Fatalf("ListOwned in the current namespace: %v", err)
	}
}
