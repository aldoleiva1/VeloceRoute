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
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"sync"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// Defaults for KernelOptions.
const (
	DefaultProtocol = 201 // route protocol id that marks routes as ours
	DefaultMetric   = 20  // route priority; see KernelOptions.Metric
	DefaultTable    = unix.RT_TABLE_MAIN
)

// KernelOptions selects which kernel routes belong to VeloceRoute.
//
// Ownership is what makes it safe to delete routes: a route is ours only if
// its protocol, table and metric all match. The kernel's own routes
// (protocol kernel, boot, static) and other daemons' routes never match.
type KernelOptions struct {
	// Table is the routing table to program. Default: the main table.
	Table int
	// Protocol is the rtnetlink protocol id stamped on our routes (shown as
	// "proto 201" by ip route). It must not be one of the kernel's own
	// (0 to 4: unspec, redirect, kernel, boot, static). Default 201.
	Protocol int
	// Metric is the route priority. It is deliberately not zero. The kernel
	// replaces an existing route with the same prefix and metric whatever its
	// protocol, so metric 0 would let a BGP route overwrite a connected or
	// static route. With our own metric the two coexist, and the connected or
	// static route (metric 0) is preferred. Default 20.
	Metric int
	// NetNS programs a network namespace other than the current one. Nil
	// means the current namespace. (It is a pointer because a namespace handle
	// is a file descriptor, so its zero value is a valid descriptor, not
	// "none".)
	NetNS *netns.NsHandle
}

type kernelProgrammer struct {
	h      *netlink.Handle
	ns     *netns.NsHandle
	table  int
	proto  netlink.RouteProtocol
	metric int

	// Interface names and indexes of link-local next hops, so a route does not
	// cost an extra netlink call. Cleared when a link changes.
	mu    sync.Mutex
	index map[string]int
	name  map[int]string
}

// NewKernelProgrammer returns a Programmer that manages Linux kernel routes
// through netlink. It needs CAP_NET_ADMIN. Close it when done.
func NewKernelProgrammer(o KernelOptions) (interface {
	Programmer
	Close() error
}, error) {
	if o.Table == 0 {
		o.Table = DefaultTable
	}
	if o.Protocol == 0 {
		o.Protocol = DefaultProtocol
	}
	if o.Metric == 0 {
		o.Metric = DefaultMetric
	}
	if o.Protocol < 5 || o.Protocol > 255 {
		return nil, fmt.Errorf("route protocol %d is not usable: use 5 to 255 (0 to 4 are the kernel's own)", o.Protocol)
	}
	if o.Table < 1 || o.Table == unix.RT_TABLE_LOCAL {
		return nil, fmt.Errorf("routing table %d is not usable", o.Table)
	}
	if o.Metric < 1 {
		return nil, fmt.Errorf("route metric %d is not usable: it must be at least 1", o.Metric)
	}
	var h *netlink.Handle
	var err error
	if o.NetNS != nil {
		h, err = netlink.NewHandleAt(*o.NetNS)
	} else {
		h, err = netlink.NewHandle()
	}
	if err != nil {
		return nil, fmt.Errorf("open netlink: %w", err)
	}
	return &kernelProgrammer{h: h, ns: o.NetNS, table: o.Table, proto: netlink.RouteProtocol(o.Protocol), metric: o.Metric,
		index: map[string]int{}, name: map[int]string{}}, nil
}

func (k *kernelProgrammer) Close() error {
	k.h.Close()
	return nil
}

func ipNet(p netip.Prefix) *net.IPNet {
	a := p.Addr()
	return &net.IPNet{IP: net.IP(a.AsSlice()), Mask: net.CIDRMask(p.Bits(), a.BitLen())}
}

func (k *kernelProgrammer) route(p netip.Prefix) *netlink.Route {
	return &netlink.Route{Dst: ipNet(p), Table: k.table, Protocol: k.proto, Priority: k.metric}
}

// linkIndex finds the interface index for a zone: a name, or a number.
func (k *kernelProgrammer) linkIndex(zone string) (int, error) {
	if n, err := strconv.Atoi(zone); err == nil {
		return n, nil
	}
	k.mu.Lock()
	idx, ok := k.index[zone]
	k.mu.Unlock()
	if ok {
		return idx, nil
	}
	l, err := k.h.LinkByName(zone)
	if err != nil {
		return 0, fmt.Errorf("%w: interface %q: %v", ErrNextHopUnreachable, zone, err)
	}
	idx = l.Attrs().Index
	k.mu.Lock()
	k.index[zone] = idx
	k.name[idx] = zone
	k.mu.Unlock()
	return idx, nil
}

// zoneOf is the interface name for an index (the index itself if the link is
// gone).
func (k *kernelProgrammer) zoneOf(idx int) string {
	k.mu.Lock()
	name, ok := k.name[idx]
	k.mu.Unlock()
	if ok {
		return name
	}
	l, err := k.h.LinkByIndex(idx)
	if err != nil {
		return strconv.Itoa(idx)
	}
	name = l.Attrs().Name
	k.mu.Lock()
	k.index[name] = idx
	k.name[idx] = name
	k.mu.Unlock()
	return name
}

func (k *kernelProgrammer) clearLinkCache() {
	k.mu.Lock()
	clear(k.index)
	clear(k.name)
	k.mu.Unlock()
}

// gateway converts a next hop into its gateway address and interface index
// (0 when the kernel should work the interface out itself).
func (k *kernelProgrammer) gateway(h netip.Addr) (net.IP, int, error) {
	if h.Is6() && h.IsLinkLocalUnicast() {
		if h.Zone() == "" {
			return nil, 0, fmt.Errorf("link-local next hop %s has no interface", h)
		}
		idx, err := k.linkIndex(h.Zone())
		return net.IP(h.WithZone("").AsSlice()), idx, err
	}
	return net.IP(h.WithZone("").AsSlice()), 0, nil
}

func (k *kernelProgrammer) Replace(r Route) error {
	if len(r.NextHops) == 0 {
		return errors.New("route has no next hop")
	}
	nl := k.route(r.Prefix)
	if len(r.NextHops) == 1 {
		gw, idx, err := k.gateway(r.NextHops[0])
		if err != nil {
			return err
		}
		nl.Gw, nl.LinkIndex = gw, idx
	} else {
		// ECMP: one route with several next hops, replaced as a whole so the
		// kernel never forwards over a half-updated set.
		for _, h := range r.NextHops {
			gw, idx, err := k.gateway(h)
			if err != nil {
				return err
			}
			nl.MultiPath = append(nl.MultiPath, &netlink.NexthopInfo{Gw: gw, LinkIndex: idx})
		}
	}
	if err := k.h.RouteReplace(nl); err != nil {
		if errors.Is(err, unix.ENETUNREACH) || errors.Is(err, unix.ENODEV) {
			return fmt.Errorf("%w: %v", ErrNextHopUnreachable, err)
		}
		return err
	}
	return nil
}

func (k *kernelProgrammer) Delete(p netip.Prefix) error {
	err := k.h.RouteDel(k.route(p))
	if err != nil && errors.Is(err, unix.ESRCH) {
		return nil // already gone
	}
	return err
}

func (k *kernelProgrammer) ListOwned() ([]Route, error) {
	filter := &netlink.Route{Table: k.table, Protocol: k.proto}
	rs, err := k.h.RouteListFiltered(netlink.FAMILY_ALL, filter, netlink.RT_FILTER_TABLE|netlink.RT_FILTER_PROTOCOL)
	if err != nil {
		return nil, err
	}
	var out []Route
	for _, r := range rs {
		if r.Priority != k.metric {
			continue
		}
		p, ok := prefixOf(r)
		if !ok {
			continue
		}
		var hops []netip.Addr
		if len(r.MultiPath) > 0 {
			for _, n := range r.MultiPath {
				if a, ok := k.hopOf(n.Gw, n.LinkIndex); ok {
					hops = append(hops, a)
				}
			}
		} else if a, ok := k.hopOf(r.Gw, r.LinkIndex); ok {
			hops = append(hops, a)
		}
		slices.SortFunc(hops, netip.Addr.Compare)
		out = append(out, Route{Prefix: p, NextHops: hops})
	}
	return out, nil
}

// hopOf is gateway's inverse: a link-local gateway gets its interface back as
// the zone, so what we list compares equal to what we asked for.
func (k *kernelProgrammer) hopOf(gw net.IP, ifindex int) (netip.Addr, bool) {
	a, ok := netip.AddrFromSlice(gw)
	if !ok {
		return netip.Addr{}, false
	}
	a = a.Unmap()
	if a.Is6() && a.IsLinkLocalUnicast() && ifindex != 0 {
		a = a.WithZone(k.zoneOf(ifindex))
	}
	return a, true
}

// Lookup finds our route for the prefix. The kernel can only list routes, not
// fetch one by exact prefix, so this reads all of ours.
func (k *kernelProgrammer) Lookup(p netip.Prefix) (Route, bool, error) {
	owned, err := k.ListOwned()
	if err != nil {
		return Route{}, false, err
	}
	var out Route
	found := false
	for _, r := range owned {
		if r.Prefix != p {
			continue
		}
		found = true
		out.Prefix = p
		out.NextHops = append(out.NextHops, r.NextHops...)
	}
	slices.SortFunc(out.NextHops, netip.Addr.Compare)
	return out, found, nil
}

// changeBuffer is how many Changes can wait for the syncer before some are
// dropped (the syncer is then told to check everything).
const changeBuffer = 256

// Changes subscribes to route, address and link changes of the namespace we
// program and turns them into Changes. Our own route installs are ignored;
// the removal of one of our routes is reported with its prefix.
//
// The kernel drops routes silently when what they depend on goes away: when an
// address is removed or a link goes down it deletes our routes through it
// without sending a notification for them (it does for the connected route).
// So an address, a link or a removed kernel route is reported as Resync, which
// makes the syncer compare its routes with the kernel's in one pass.
func (k *kernelProgrammer) Changes(ctx context.Context) (<-chan Change, error) {
	done := make(chan struct{})
	lost := make(chan struct{}, 1) // a subscription reported an error: events may be missing
	onErr := func(error) {
		select {
		case lost <- struct{}{}:
		default:
		}
	}
	routes := make(chan netlink.RouteUpdate, changeBuffer)
	addrs := make(chan netlink.AddrUpdate, changeBuffer)
	links := make(chan netlink.LinkUpdate, changeBuffer)
	const rcvbuf = 4 << 20 // a flush of a big table is a burst of route messages
	if err := netlink.RouteSubscribeWithOptions(routes, done, netlink.RouteSubscribeOptions{Namespace: k.ns, ErrorCallback: onErr, ReceiveBufferSize: rcvbuf}); err != nil {
		close(done)
		return nil, err
	}
	if err := netlink.AddrSubscribeWithOptions(addrs, done, netlink.AddrSubscribeOptions{Namespace: k.ns, ErrorCallback: onErr}); err != nil {
		close(done)
		return nil, err
	}
	if err := netlink.LinkSubscribeWithOptions(links, done, netlink.LinkSubscribeOptions{Namespace: k.ns, ErrorCallback: onErr}); err != nil {
		close(done)
		return nil, err
	}
	out := make(chan Change, changeBuffer)
	var dropped bool
	send := func(c Change) {
		if dropped {
			// Tell the syncer first that it has missed something.
			select {
			case out <- Change{Resync: true}:
				dropped = false
			default:
			}
		}
		select {
		case out <- c:
		default:
			dropped = true
		}
	}
	go func() {
		defer close(out)
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-lost:
				send(Change{Resync: true})
			case u, ok := <-routes:
				if !ok {
					return
				}
				r := u.Route
				if r.Table == k.table && r.Protocol == k.proto && r.Priority == k.metric {
					if u.Type == unix.RTM_DELROUTE {
						if p, ok := prefixOf(r); ok {
							send(Change{Prefix: p})
						}
					}
					continue
				}
				switch {
				case u.Type == unix.RTM_NEWROUTE:
					send(Change{Reach: true})
				case r.Protocol == unix.RTPROT_KERNEL:
					// A connected route went: routes through it may have been
					// dropped without a word.
					send(Change{Resync: true})
				}
			case u, ok := <-addrs:
				if !ok {
					return
				}
				send(Change{Reach: u.NewAddr, Resync: true})
			case u, ok := <-links:
				if !ok {
					return
				}
				k.clearLinkCache()
				send(Change{Reach: u.Attrs() != nil && u.Attrs().Flags&net.FlagUp != 0, Resync: true})
			}
		}
	}()
	return out, nil
}

// prefixOf converts a listed route to a prefix. netlink reports a default
// route with a nil Dst, so the family comes from the gateway or the route's
// own family.
func prefixOf(r netlink.Route) (netip.Prefix, bool) {
	if r.Dst == nil {
		if r.Family == netlink.FAMILY_V6 {
			return netip.PrefixFrom(netip.IPv6Unspecified(), 0), true
		}
		return netip.PrefixFrom(netip.IPv4Unspecified(), 0), true
	}
	addr, ok := netip.AddrFromSlice(r.Dst.IP)
	if !ok {
		return netip.Prefix{}, false
	}
	ones, _ := r.Dst.Mask.Size()
	return netip.PrefixFrom(addr.Unmap(), ones), true
}

// CheckPrivileges reports whether this process may change kernel routes, that
// is, whether CAP_NET_ADMIN is in its effective set. Checking at start-up turns
// a missing capability into one clear error instead of a failure per route.
func CheckPrivileges() error {
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		return fmt.Errorf("read capabilities: %w", err)
	}
	const capNetAdmin = 12
	if data[0].Effective&(1<<capNetAdmin) == 0 {
		return errors.New("kernel route sync needs CAP_NET_ADMIN: run as root or grant the capability")
	}
	return nil
}
