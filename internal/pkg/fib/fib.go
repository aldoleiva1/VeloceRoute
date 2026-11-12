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

// Package fib copies BGP best paths into the operating system's forwarding
// table. The sync logic here is portable and talks to a Programmer; the Linux
// Programmer (kernel_linux.go) uses netlink.
//
// Scope today (roadmap task E4-1): one next hop per prefix, IPv4 and IPv6
// unicast, routes learned from peers only. Next hop resolution, ECMP and
// bulk programming are tasks E4-2, E4-3 and E4-5.
package fib

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"

	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
)

// Route is what gets programmed: a prefix and the gateway to reach it.
type Route struct {
	Prefix  netip.Prefix
	NextHop netip.Addr
}

func (r Route) String() string { return fmt.Sprintf("%s via %s", r.Prefix, r.NextHop) }

// ErrNextHopUnreachable means the operating system cannot reach the next hop
// (it is not on a connected network). The route is skipped, not retried.
var ErrNextHopUnreachable = errors.New("next hop is not reachable")

// ErrKernelUnsupported is returned where there is no kernel route programmer
// (anything but Linux).
var ErrKernelUnsupported = errors.New("kernel route sync is only supported on Linux")

// Programmer installs and removes routes. Implementations must only ever touch
// routes they own, so that deleting a route can never remove one that a person
// or another daemon installed.
type Programmer interface {
	// Replace installs the route, replacing an existing route of ours for the
	// same prefix.
	Replace(Route) error
	// Delete removes our route for the prefix. Deleting a route that is not
	// there is not an error.
	Delete(netip.Prefix) error
	// ListOwned returns every route this programmer owns, for cleanup of
	// routes left behind by an earlier run.
	ListOwned() ([]Route, error)
}

// Stats counts what the syncer has done, for logs and tests.
type Stats struct {
	Installed   int // routes currently installed
	Replaced    uint64
	Deleted     uint64
	Unreachable uint64 // next hop not reachable, route skipped
	Errors      uint64 // other programmer errors
	Skipped     uint64 // paths not eligible (local, unsupported family, no next hop)
}

// Syncer turns best path events into Programmer calls. All state is guarded by
// a mutex; events are normally delivered from one goroutine, which keeps the
// kernel table a single-writer copy of the best paths.
type Syncer struct {
	prog   Programmer
	logger *slog.Logger

	mu        sync.Mutex
	installed map[netip.Prefix]netip.Addr
	stats     Stats
	// failed remembers prefixes whose last install failed, so a later
	// withdrawal does not try to delete a route that was never installed
	// and a repeated failure is logged once.
	failed map[netip.Prefix]struct{}
	// stopped is set by RemoveAll: a best path event that is still in flight
	// when shutdown starts must not install a route after cleanup.
	stopped bool
}

func NewSyncer(prog Programmer, logger *slog.Logger) *Syncer {
	return &Syncer{
		prog:      prog,
		logger:    logger,
		installed: map[netip.Prefix]netip.Addr{},
		failed:    map[netip.Prefix]struct{}{},
	}
}

// Stats returns a snapshot of the counters.
func (s *Syncer) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stats
	st.Installed = len(s.installed)
	return st
}

// Reconcile removes routes left by an earlier run. Call it before the first
// best path arrives: at that point no session is up, so nothing in the old
// kernel routes can still be valid.
func (s *Syncer) Reconcile() error {
	stale, err := s.prog.ListOwned()
	if err != nil {
		return fmt.Errorf("list owned routes: %w", err)
	}
	var errs []error
	for _, r := range stale {
		if err := s.prog.Delete(r.Prefix); err != nil {
			errs = append(errs, fmt.Errorf("delete stale %s: %w", r.Prefix, err))
			continue
		}
		s.logger.Info("removed stale route", slog.String("route", r.String()))
	}
	return errors.Join(errs...)
}

// RemoveAll deletes every route this syncer installed and stops the syncer:
// later events are ignored. It keeps going after an error and returns them
// together.
func (s *Syncer) RemoveAll() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	var errs []error
	for p := range s.installed {
		if err := s.prog.Delete(p); err != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", p, err))
			continue
		}
		s.stats.Deleted++
		delete(s.installed, p)
	}
	return errors.Join(errs...)
}

// HandleBestPaths applies one best path event.
func (s *Syncer) HandleBestPaths(paths []*apiutil.Path) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	for _, p := range paths {
		s.handle(p)
	}
}

func (s *Syncer) handle(p *apiutil.Path) {
	prefix, nh, disp := classify(p)
	switch disp {
	case dispIgnore:
		s.stats.Skipped++
	case dispRemove:
		// A withdrawal, or a best path we do not program (locally originated,
		// unusable next hop). Either way any route of ours for the prefix is
		// no longer the best path and must go.
		if !p.Withdrawal {
			s.stats.Skipped++
		}
		s.withdraw(prefix)
	case dispProgram:
		s.program(prefix, nh)
	}
}

func (s *Syncer) program(prefix netip.Prefix, nh netip.Addr) {
	if cur, have := s.installed[prefix]; have && cur == nh {
		return // nothing changed
	}
	r := Route{Prefix: prefix, NextHop: nh}
	if err := s.prog.Replace(r); err != nil {
		// The previous route for this prefix (if any) points at a next hop
		// that is no longer the best path, so remove it rather than keep
		// forwarding along stale information.
		s.noteFailure(prefix, r, err)
		s.withdraw(prefix)
		s.failed[prefix] = struct{}{}
		return
	}
	delete(s.failed, prefix)
	s.installed[prefix] = nh
	s.stats.Replaced++
	s.logger.Debug("route installed", slog.String("route", r.String()))
}

func (s *Syncer) noteFailure(prefix netip.Prefix, r Route, err error) {
	_, already := s.failed[prefix]
	if errors.Is(err, ErrNextHopUnreachable) {
		s.stats.Unreachable++
		if !already {
			s.logger.Warn("route skipped, next hop not reachable", slog.String("route", r.String()))
		}
		return
	}
	s.stats.Errors++
	if !already {
		s.logger.Error("failed to install route", slog.String("route", r.String()), slog.String("error", err.Error()))
	}
}

func (s *Syncer) withdraw(prefix netip.Prefix) {
	delete(s.failed, prefix)
	if _, have := s.installed[prefix]; !have {
		return // never installed (or install failed): nothing of ours to remove
	}
	if err := s.prog.Delete(prefix); err != nil {
		s.stats.Errors++
		s.logger.Error("failed to delete route", slog.String("prefix", prefix.String()), slog.String("error", err.Error()))
		return
	}
	delete(s.installed, prefix)
	s.stats.Deleted++
	s.logger.Debug("route removed", slog.String("prefix", prefix.String()))
}

type disposition int

const (
	dispIgnore  disposition = iota // not a path this package handles
	dispProgram                    // install this route
	dispRemove                     // remove our route for the prefix, if any
)

// classify decides what a best path event means for the kernel table.
//
//   - Only unicast IPv4 and IPv6 are handled; anything else is ignored.
//   - A withdrawal removes the route.
//   - A path with no peer is one this router originated (its next hop is
//     ourselves), a next hop GoBGP marked invalid, or a next hop of a different
//     address family than the prefix cannot be programmed; if such a path is
//     now the best, a route we installed earlier is stale, so it is removed.
func classify(p *apiutil.Path) (netip.Prefix, netip.Addr, disposition) {
	if p == nil {
		return netip.Prefix{}, netip.Addr{}, dispIgnore
	}
	var prefix netip.Prefix
	switch p.Family {
	case bgp.RF_IPv4_UC, bgp.RF_IPv6_UC:
		n, ok := p.Nlri.(*bgp.IPAddrPrefix)
		if !ok {
			return netip.Prefix{}, netip.Addr{}, dispIgnore
		}
		prefix = n.Prefix
	default:
		return netip.Prefix{}, netip.Addr{}, dispIgnore
	}
	if p.Withdrawal || !p.PeerAddress.IsValid() || p.IsNexthopInvalid {
		return prefix, netip.Addr{}, dispRemove
	}
	nh := nextHopOf(p)
	if !nh.IsValid() || nh.IsUnspecified() || nh.Is4() != prefix.Addr().Is4() {
		// For example an IPv4 prefix with an IPv6 next hop needs a
		// different kernel encoding; not supported yet.
		return prefix, netip.Addr{}, dispRemove
	}
	return prefix, nh, dispProgram
}

func nextHopOf(p *apiutil.Path) netip.Addr {
	for _, a := range p.Attrs {
		switch v := a.(type) {
		case *bgp.PathAttributeNextHop:
			return v.Value
		case *bgp.PathAttributeMpReachNLRI:
			if v.Nexthop.IsValid() && !v.Nexthop.IsUnspecified() {
				return v.Nexthop
			}
			// A peer with no global address sends only a link-local one.
			return v.LinkLocalNexthop
		}
	}
	return netip.Addr{}
}
