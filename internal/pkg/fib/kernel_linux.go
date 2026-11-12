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
	"net"
	"net/netip"
	"slices"

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
	table  int
	proto  netlink.RouteProtocol
	metric int
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
	return &kernelProgrammer{h: h, table: o.Table, proto: netlink.RouteProtocol(o.Protocol), metric: o.Metric}, nil
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

func (k *kernelProgrammer) Replace(r Route) error {
	if len(r.NextHops) == 0 {
		return errors.New("route has no next hop")
	}
	nl := k.route(r.Prefix)
	if len(r.NextHops) == 1 {
		nl.Gw = net.IP(r.NextHops[0].AsSlice())
	} else {
		// ECMP: one route with several next hops, replaced as a whole so the
		// kernel never forwards over a half-updated set.
		for _, h := range r.NextHops {
			nl.MultiPath = append(nl.MultiPath, &netlink.NexthopInfo{Gw: net.IP(h.AsSlice())})
		}
	}
	if err := k.h.RouteReplace(nl); err != nil {
		if errors.Is(err, unix.ENETUNREACH) {
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
				if a, ok := netip.AddrFromSlice(n.Gw); ok {
					hops = append(hops, a.Unmap())
				}
			}
		} else if a, ok := netip.AddrFromSlice(r.Gw); ok {
			hops = append(hops, a.Unmap())
		}
		slices.SortFunc(hops, netip.Addr.Compare)
		out = append(out, Route{Prefix: p, NextHops: hops})
	}
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
