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
// Scope today (roadmap tasks E4-1 and E4-3): IPv4 and IPv6 unicast, routes
// learned from peers only, one next hop per prefix or, when the BGP server
// runs with multiple paths, an ECMP route with every equal-cost next hop.
// Next hop resolution and bulk programming are tasks E4-2 and E4-5.
package fib

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync"

	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
)

// Route is what gets programmed: a prefix and the gateways to reach it. One
// next hop is an ordinary route; several make an ECMP route that spreads
// traffic over all of them. NextHops is sorted and has no duplicates.
type Route struct {
	Prefix   netip.Prefix
	NextHops []netip.Addr
}

func (r Route) String() string {
	hops := make([]string, len(r.NextHops))
	for i, h := range r.NextHops {
		hops[i] = h.String()
	}
	return fmt.Sprintf("%s via %s", r.Prefix, strings.Join(hops, ", "))
}

// DefaultMaxNextHops caps the next hops of one ECMP route when the syncer is
// given no other limit.
const DefaultMaxNextHops = 64

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
	// same prefix (all of its next hops at once). If any next hop cannot be
	// reached it fails with ErrNextHopUnreachable and installs nothing new.
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

// source identifies where a path came from: the peer and, with add-path, the
// path identifier that peer assigned.
type source struct {
	peer netip.Addr
	id   uint32
}

// candidate is one eligible path of a prefix.
type candidate struct {
	src source
	nh  netip.Addr
}

// entry is what the syncer knows about one prefix.
type entry struct {
	// cands are the eligible paths as last reported. With one best path per
	// prefix it holds at most one; with multiple paths, one per equal-cost path.
	cands []candidate
	// applied is the next hop set the last Replace was asked for; installed says
	// whether the kernel holds a route of ours for the prefix. They differ from
	// the set in the kernel only when some next hops were unreachable.
	applied   []netip.Addr
	installed bool
	// failed is set when the last install failed or left out unreachable next
	// hops, so that a repeat is logged once.
	failed bool
}

// Option configures a Syncer.
type Option func(*Syncer)

// WithMultipath tells the syncer that the BGP server runs with multiple paths
// (use-multiple-paths), so its events report every equal-cost path of a prefix
// as a change to that path and not as a new best path. The syncer then builds
// one ECMP route with up to maxNextHops next hops (DefaultMaxNextHops when
// maxNextHops is below 1). Without this option it expects one best path per
// prefix, where a new path replaces the earlier one.
func WithMultipath(maxNextHops int) Option {
	return func(s *Syncer) {
		if maxNextHops < 1 {
			maxNextHops = DefaultMaxNextHops
		}
		s.multipath = true
		s.maxNextHops = maxNextHops
	}
}

// Syncer turns best path events into Programmer calls. All state is guarded by
// a mutex; events are normally delivered from one goroutine, which keeps the
// kernel table a single-writer copy of the best paths.
type Syncer struct {
	prog        Programmer
	logger      *slog.Logger
	multipath   bool
	maxNextHops int

	mu         sync.Mutex
	entries    map[netip.Prefix]entry
	dirty      []netip.Prefix // prefixes changed by the event being handled, in order
	dirtySet   map[netip.Prefix]struct{}
	nInstalled int
	stats      Stats
	// stopped is set by RemoveAll: a best path event that is still in flight
	// when shutdown starts must not install a route after cleanup.
	stopped bool
}

func NewSyncer(prog Programmer, logger *slog.Logger, opts ...Option) *Syncer {
	s := &Syncer{
		prog:        prog,
		logger:      logger,
		maxNextHops: 1,
		entries:     map[netip.Prefix]entry{},
		dirtySet:    map[netip.Prefix]struct{}{},
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Stats returns a snapshot of the counters.
func (s *Syncer) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stats
	st.Installed = s.nInstalled
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
	seen := map[netip.Prefix]struct{}{}
	for _, r := range stale {
		// A programmer may report one route per next hop; deleting by prefix
		// removes them all.
		if _, dup := seen[r.Prefix]; dup {
			continue
		}
		seen[r.Prefix] = struct{}{}
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
	for p, e := range s.entries {
		if !e.installed {
			delete(s.entries, p)
			continue
		}
		if err := s.prog.Delete(p); err != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", p, err))
			continue
		}
		s.stats.Deleted++
		s.nInstalled--
		delete(s.entries, p)
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
	// Program each prefix once, from the state after the whole event, so
	// several paths of one prefix in an event make one kernel change and the
	// kernel never sees a half-built ECMP set.
	for _, prefix := range s.dirty {
		s.apply(prefix, s.entries[prefix])
	}
	s.dirty = s.dirty[:0]
	clear(s.dirtySet)
}

func (s *Syncer) handle(p *apiutil.Path) {
	prefix, nh, disp := classify(p)
	if disp == dispIgnore {
		s.stats.Skipped++
		return
	}
	src := source{peer: p.PeerAddress, id: p.RemoteID}
	e := s.entries[prefix]
	switch disp {
	case dispRemove:
		// A withdrawal, or a path we do not program (locally originated,
		// unusable next hop).
		if !p.Withdrawal {
			s.stats.Skipped++
		}
		if s.multipath {
			// The event is about one path of the prefix; the others stay.
			e.cands = removeCandidate(e.cands, src)
		} else {
			// The event is about the best path, so any route of ours for
			// the prefix is no longer the best path and must go.
			e.cands = e.cands[:0]
		}
	case dispProgram:
		if s.multipath {
			e.cands = upsertCandidate(e.cands, candidate{src, nh})
		} else {
			e.cands = append(e.cands[:0], candidate{src, nh})
		}
	}
	s.entries[prefix] = e
	if _, seen := s.dirtySet[prefix]; !seen {
		s.dirtySet[prefix] = struct{}{}
		s.dirty = append(s.dirty, prefix)
	}
}

func removeCandidate(cs []candidate, src source) []candidate {
	for i, c := range cs {
		if c.src == src {
			cs[i] = cs[len(cs)-1]
			return cs[:len(cs)-1]
		}
	}
	return cs
}

func upsertCandidate(cs []candidate, c candidate) []candidate {
	for i := range cs {
		if cs[i].src == c.src {
			cs[i] = c
			return cs
		}
	}
	return append(cs, c)
}

// wanted returns the sorted, de-duplicated next hops of the candidates, cut to
// the configured maximum (the lowest addresses are kept, so the choice does
// not change from one event to the next).
func (s *Syncer) wanted(prefix netip.Prefix, cs []candidate) []netip.Addr {
	if len(cs) == 0 {
		return nil
	}
	hops := make([]netip.Addr, 0, len(cs))
	for _, c := range cs {
		hops = append(hops, c.nh)
	}
	slices.SortFunc(hops, netip.Addr.Compare)
	hops = slices.Compact(hops)
	if len(hops) > s.maxNextHops {
		s.logger.Warn("too many equal-cost next hops, using the lowest addresses",
			slog.String("prefix", prefix.String()), slog.Int("next_hops", len(hops)), slog.Int("max", s.maxNextHops))
		hops = hops[:s.maxNextHops]
	}
	return hops
}

// apply makes the kernel route for a prefix match its candidates and stores
// the entry.
func (s *Syncer) apply(prefix netip.Prefix, e entry) {
	want := s.wanted(prefix, e.cands)
	switch {
	case len(want) == 0:
		e.failed = false
		e.applied = nil
		if e.installed {
			if err := s.prog.Delete(prefix); err != nil {
				s.stats.Errors++
				s.logger.Error("failed to delete route", slog.String("prefix", prefix.String()), slog.String("error", err.Error()))
				s.entries[prefix] = e // keep tracking it so RemoveAll can retry
				return
			}
			s.stats.Deleted++
			s.nInstalled--
			s.logger.Debug("route removed", slog.String("prefix", prefix.String()))
		}
		delete(s.entries, prefix)
	case e.installed && slices.Equal(want, e.applied):
		s.entries[prefix] = e // nothing changed
	default:
		s.install(prefix, want, e)
	}
}

func (s *Syncer) install(prefix netip.Prefix, want []netip.Addr, e entry) {
	r := Route{Prefix: prefix, NextHops: want}
	err := s.prog.Replace(r)
	var skipped []netip.Addr
	if err != nil && errors.Is(err, ErrNextHopUnreachable) && len(want) > 1 {
		// One unreachable next hop must not take the reachable ones down with
		// it: program the ones that work.
		skipped, err = s.installReachable(r)
	}
	if err != nil {
		s.noteFailure(prefix, r, err, e.failed)
		e.failed = true
		e.applied = nil
		// The old route (if any) points at paths that are no longer the
		// answer, so remove it rather than keep forwarding along stale
		// information.
		if e.installed {
			if derr := s.prog.Delete(prefix); derr != nil {
				s.stats.Errors++
				s.logger.Error("failed to delete route", slog.String("prefix", prefix.String()), slog.String("error", derr.Error()))
			} else {
				s.stats.Deleted++
				s.nInstalled--
				e.installed = false
			}
		}
		s.entries[prefix] = e
		return
	}
	if len(skipped) > 0 {
		s.stats.Unreachable += uint64(len(skipped))
		if !e.failed {
			s.logger.Warn("some next hops not reachable, route installed with the others",
				slog.String("prefix", prefix.String()), slog.Any("unreachable", skipped))
		}
	}
	if !e.installed {
		s.nInstalled++
	}
	e.installed = true
	e.failed = len(skipped) > 0 // a partial route is not retried noisily
	e.applied = want
	s.stats.Replaced++
	s.entries[prefix] = e
	s.logger.Debug("route installed", slog.String("route", r.String()))
}

// installReachable is the fallback for an ECMP route the kernel refused
// because some next hop is unreachable. It finds the reachable next hops by
// trying each one alone, then installs those together. It returns the next
// hops that were left out. Only on the failure path does this cost extra
// kernel calls.
func (s *Syncer) installReachable(r Route) (skipped []netip.Addr, err error) {
	var ok []netip.Addr
	var firstErr error
	for _, h := range r.NextHops {
		perr := s.prog.Replace(Route{Prefix: r.Prefix, NextHops: []netip.Addr{h}})
		switch {
		case perr == nil:
			ok = append(ok, h)
		case errors.Is(perr, ErrNextHopUnreachable):
			skipped = append(skipped, h)
			if firstErr == nil {
				firstErr = perr
			}
		default:
			s.cleanProbe(r.Prefix, len(ok) > 0)
			return nil, perr
		}
	}
	switch {
	case len(ok) == 0:
		return nil, firstErr
	case len(ok) == 1:
		return skipped, nil // the route the last probe left is the answer
	}
	if err := s.prog.Replace(Route{Prefix: r.Prefix, NextHops: ok}); err != nil {
		s.cleanProbe(r.Prefix, true)
		return nil, err
	}
	return skipped, nil
}

// cleanProbe removes the single-next-hop route a probe left behind when the
// ECMP install then failed for another reason.
func (s *Syncer) cleanProbe(prefix netip.Prefix, left bool) {
	if left {
		_ = s.prog.Delete(prefix)
	}
}

func (s *Syncer) noteFailure(prefix netip.Prefix, r Route, err error, already bool) {
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
