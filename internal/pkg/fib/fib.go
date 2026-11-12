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
// Scope today (roadmap tasks E4-1, E4-2 and E4-3): IPv4 and IPv6 unicast,
// routes learned from peers only, one next hop per prefix or, when the BGP
// server runs with multiple paths, an ECMP route with every equal-cost next
// hop. Next hops that are not reachable yet are retried when the network
// changes, a route the kernel drops is put back, and an IPv6 link-local next
// hop is bound to the peer's interface. Bulk programming is task E4-5.
package fib

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"
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

// DefaultRetryInterval is how often Run retries routes that are waiting for a
// next hop to become reachable, as a safety net beside change notifications.
const DefaultRetryInterval = 30 * time.Second

// ErrNextHopUnreachable means the operating system cannot reach the next hop
// (it is not on a connected network, or its interface is missing). The route is
// not installed now; the syncer retries when the network changes.
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
	// Lookup returns our route for the prefix as it is in the operating
	// system now, and whether there is one.
	Lookup(netip.Prefix) (Route, bool, error)
}

// Op is one change in a batch: install Route, or with Delete remove the route
// for Route.Prefix.
type Op struct {
	Delete bool
	Route  Route
}

// BatchProgrammer is a Programmer that can apply many changes with far fewer
// system calls than one call per route. Apply runs the ops in order, a failing
// op does not stop the others, and errs[i] is the result of ops[i] with the
// same meaning as Replace or Delete (so deleting a route that is not there is
// not an error). The syncer uses it when it has several changes at once.
type BatchProgrammer interface {
	Programmer
	Apply(ops []Op) (errs []error)
}

// Change is a hint that the operating system's routing state moved under the
// syncer. It is only a hint: the syncer checks the real state before acting.
type Change struct {
	// Prefix is valid when one of our routes for the prefix was removed.
	Prefix netip.Prefix
	// Reach is set when something happened that can make a next hop reachable
	// (a link came up, an address or a route was added).
	Reach bool
	// Resync is set when routes of ours may have been dropped without a
	// notification (an address or link went away) or when notifications may
	// have been lost: check everything.
	Resync bool
}

// ChangeSource reports Changes until ctx is cancelled, when it closes the
// channel. Delivery may drop events, but then it sends a Resync.
type ChangeSource interface {
	Changes(ctx context.Context) (<-chan Change, error)
}

// Stats counts what the syncer has done, for logs and tests.
type Stats struct {
	Installed   int // routes currently installed
	Replaced    uint64
	Deleted     uint64
	Unreachable uint64 // next hop not reachable, route skipped
	Recovered   uint64 // routes installed by a retry after their next hop became reachable
	Drifted     uint64 // routes the kernel dropped or changed behind our back, restored
	Batches     uint64 // batched kernel calls made (see BatchProgrammer)
	BatchedOps  uint64 // changes sent in those batches
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
	// whether the kernel holds a route of ours for the prefix. live is what is
	// in the kernel when that is less than applied (some next hops were
	// unreachable); nil means all of applied.
	applied   []netip.Addr
	live      []netip.Addr
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

// WithDebounce sets how long Run waits to batch a burst of changes. Tests use a
// short time.
func WithDebounce(d time.Duration) Option { return func(s *Syncer) { s.debounce = d } }

// Syncer turns best path events into Programmer calls. All state is guarded by
// a mutex; events are normally delivered from one goroutine, which keeps the
// kernel table a single-writer copy of the best paths.
type Syncer struct {
	prog        Programmer
	logger      *slog.Logger
	multipath   bool
	maxNextHops int
	debugLogs   bool // per-route debug logging is on; checked once so a route costs no formatting when it is off

	debounce time.Duration

	// Events from the BGP watcher wait here for the writer goroutine, which
	// applies everything that piled up while it was busy as one batch.
	qmu   sync.Mutex
	queue []event
	qwake chan struct{}
	qpeak int

	mu      sync.Mutex
	entries map[netip.Prefix]entry
	// pending holds prefixes that should be retried when the network changes:
	// no route could be installed for them, or only some of the next hops.
	pending    map[netip.Prefix]struct{}
	noZoneWarn map[netip.Addr]struct{} // peers already warned about a link-local next hop without interface
	dirty      []netip.Prefix          // prefixes changed by the event being handled, in order
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
		pending:     map[netip.Prefix]struct{}{},
		noZoneWarn:  map[netip.Addr]struct{}{},
		debounce:    200 * time.Millisecond,
		qwake:       make(chan struct{}, 1),
	}
	for _, o := range opts {
		o(s)
	}
	s.debugLogs = logger.Enabled(context.Background(), slog.LevelDebug)
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
	var prefixes []netip.Prefix
	for _, r := range stale {
		// A programmer may report one route per next hop; deleting by prefix
		// removes them all.
		if _, dup := seen[r.Prefix]; dup {
			continue
		}
		seen[r.Prefix] = struct{}{}
		prefixes = append(prefixes, r.Prefix)
	}
	for i, err := range s.deleteAll(prefixes) {
		if err != nil {
			errs = append(errs, fmt.Errorf("delete stale %s: %w", prefixes[i], err))
		}
	}
	if n := len(prefixes); n > 0 {
		s.logger.Info("removed stale routes", slog.Int("count", n-len(errs)))
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
	prefixes := make([]netip.Prefix, 0, s.nInstalled)
	for p, e := range s.entries {
		if !e.installed {
			delete(s.entries, p)
			continue
		}
		prefixes = append(prefixes, p)
	}
	for i, err := range s.deleteAll(prefixes) {
		p := prefixes[i]
		if err != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", p, err))
			continue
		}
		s.stats.Deleted++
		s.nInstalled--
		delete(s.entries, p)
	}
	return errors.Join(errs...)
}

// deleteAll removes the routes for the prefixes, in one batch if the
// programmer can, and returns one result per prefix. It keeps going after an
// error.
func (s *Syncer) deleteAll(prefixes []netip.Prefix) []error {
	if bp, ok := s.prog.(BatchProgrammer); ok && len(prefixes) >= minBatch {
		ops := make([]Op, len(prefixes))
		for i, p := range prefixes {
			ops[i] = Op{Delete: true, Route: Route{Prefix: p}}
		}
		s.stats.Batches++
		s.stats.BatchedOps += uint64(len(ops))
		return bp.Apply(ops)
	}
	errs := make([]error, len(prefixes))
	for i, p := range prefixes {
		errs[i] = s.prog.Delete(p)
	}
	return errs
}

// event is a best path event reduced to what the syncer needs. It is much
// smaller than the path it came from, so a backlog of events costs little
// memory.
type event struct {
	prefix     netip.Prefix
	src        source
	nh         netip.Addr
	disp       disposition
	withdrawal bool
}

// makeEvent has no side effects, so it can run on the watcher's goroutine.
func makeEvent(p *apiutil.Path) event {
	prefix, nh, disp := classify(p)
	if disp == dispIgnore {
		return event{disp: dispIgnore}
	}
	return event{prefix: prefix, src: source{peer: p.PeerAddress, id: p.RemoteID}, nh: nh, disp: disp, withdrawal: p.Withdrawal}
}

// makeChangeEvent is makeEvent for the compact server event.
func makeChangeEvent(c *server.BestPathChange) event {
	prefix, nh, disp := classifyChange(c)
	if disp == dispIgnore {
		return event{disp: dispIgnore}
	}
	return event{prefix: prefix, src: source{peer: c.PeerAddress, id: c.RemoteID}, nh: nh, disp: disp, withdrawal: c.Withdrawal}
}

// SubmitChanges is Submit for the compact events of
// server.WatchBestPathChanges, which take no path copies or attribute lists.
func (s *Syncer) SubmitChanges(changes []server.BestPathChange) {
	s.qmu.Lock()
	for i := range changes {
		s.queue = append(s.queue, makeChangeEvent(&changes[i]))
	}
	if len(s.queue) > s.qpeak {
		s.qpeak = len(s.queue)
	}
	s.qmu.Unlock()
	select {
	case s.qwake <- struct{}{}:
	default:
	}
}

// HandleBestPaths applies one best path event.
func (s *Syncer) HandleBestPaths(paths []*apiutil.Path) {
	evs := make([]event, len(paths))
	for i, p := range paths {
		evs[i] = makeEvent(p)
	}
	s.handleEvents(evs)
}

// Submit queues a best path event for the writer goroutine started by Watch
// and returns at once, so the caller (the BGP watcher) never waits for the
// kernel. The writer applies everything queued while it was busy in one go, so
// the more load, the bigger the batches.
func (s *Syncer) Submit(paths []*apiutil.Path) {
	s.qmu.Lock()
	for _, p := range paths {
		s.queue = append(s.queue, makeEvent(p))
	}
	if len(s.queue) > s.qpeak {
		s.qpeak = len(s.queue)
	}
	s.qmu.Unlock()
	select {
	case s.qwake <- struct{}{}:
	default:
	}
}

// QueuePeak is the longest the event queue has been, for sizing and tests.
func (s *Syncer) QueuePeak() int {
	s.qmu.Lock()
	defer s.qmu.Unlock()
	return s.qpeak
}

// drain applies queued events until ctx ends.
func (s *Syncer) drain(ctx context.Context) {
	var batch []event
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.qwake:
		}
		for {
			s.qmu.Lock()
			batch, s.queue = s.queue, batch[:0]
			s.qmu.Unlock()
			if len(batch) == 0 {
				break
			}
			s.handleEvents(batch)
			if ctx.Err() != nil {
				return
			}
		}
	}
}

func (s *Syncer) handleEvents(evs []event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	for i := range evs {
		s.handle(&evs[i])
	}
	// Program each prefix once, from the state after the whole event, so
	// several paths of one prefix in an event make one kernel change and the
	// kernel never sees a half-built ECMP set.
	s.flush()
	s.dirty = s.dirty[:0]
	clear(s.dirtySet)
}

func (s *Syncer) handle(ev *event) {
	if ev.disp == dispIgnore {
		s.stats.Skipped++
		return
	}
	prefix, src, nh := ev.prefix, ev.src, ev.nh
	e := s.entries[prefix]
	switch ev.disp {
	case dispRemove, dispNoInterface:
		// A withdrawal, or a path we do not program (locally originated,
		// unusable next hop).
		if ev.disp == dispNoInterface {
			if _, warned := s.noZoneWarn[src.peer]; !warned {
				s.noZoneWarn[src.peer] = struct{}{}
				s.logger.Warn("routes from this peer have a link-local next hop but the peer has no interface, not programming them",
					slog.String("peer", src.peer.String()), slog.String("next_hop", nh.String()))
			}
		}
		if !ev.withdrawal {
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

// minBatch is the fewest changes worth a batched call.
const minBatch = 2

// planned is a change waiting for a batch result.
type planned struct {
	prefix netip.Prefix
	want   []netip.Addr // nil for a delete
	before int          // next hops in the kernel route before an install
}

// flush programs the dirty prefixes. With a BatchProgrammer and enough
// changes it sends them in one batch and finishes the common case (the kernel
// accepted the change) without further calls; any prefix whose change was
// refused goes through the ordinary one-at-a-time path, which knows how to
// handle each kind of refusal.
func (s *Syncer) flush() {
	bp, ok := s.prog.(BatchProgrammer)
	if !ok || len(s.dirty) < minBatch {
		for _, prefix := range s.dirty {
			s.apply(prefix, s.entries[prefix])
		}
		return
	}
	var (
		plan []planned
		ops  []Op
	)
	for _, prefix := range s.dirty {
		e := s.entries[prefix]
		want := s.wanted(prefix, e.cands)
		switch {
		case len(want) == 0 && !e.installed:
			s.apply(prefix, e) // nothing in the kernel: bookkeeping only
		case len(want) == 0:
			plan = append(plan, planned{prefix: prefix})
			ops = append(ops, Op{Delete: true, Route: Route{Prefix: prefix}})
		case e.installed && slices.Equal(want, e.applied):
			// nothing changed
		default:
			before := 0
			if e.installed {
				before = len(e.applied)
				if e.live != nil {
					before = len(e.live)
				}
			}
			plan = append(plan, planned{prefix: prefix, want: want, before: before})
			ops = append(ops, Op{Route: Route{Prefix: prefix, NextHops: want}})
		}
	}
	if len(ops) == 0 {
		return
	}
	errs := bp.Apply(ops)
	s.stats.Batches++
	s.stats.BatchedOps += uint64(len(ops))
	for i, pl := range plan {
		e := s.entries[pl.prefix]
		if errs[i] != nil {
			s.apply(pl.prefix, e) // the one-at-a-time path repeats the change and deals with the refusal
			continue
		}
		if pl.want == nil {
			s.recordDelete(pl.prefix, e)
		} else {
			s.recordInstall(pl.prefix, e, pl.want, pl.want, nil, pl.before)
		}
	}
}

// recordDelete is the bookkeeping after the kernel removed a route.
func (s *Syncer) recordDelete(prefix netip.Prefix, e entry) {
	delete(s.pending, prefix)
	s.stats.Deleted++
	s.nInstalled--
	delete(s.entries, prefix)
	if s.debugLogs {
		s.logger.Debug("route removed", slog.String("prefix", prefix.String()))
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
		e.applied, e.live = nil, nil
		delete(s.pending, prefix)
		if e.installed {
			if err := s.prog.Delete(prefix); err != nil {
				s.stats.Errors++
				s.logger.Error("failed to delete route", slog.String("prefix", prefix.String()), slog.String("error", err.Error()))
				s.entries[prefix] = e // keep tracking it so RemoveAll can retry
				return
			}
			s.stats.Deleted++
			s.nInstalled--
			if s.debugLogs {
				s.logger.Debug("route removed", slog.String("prefix", prefix.String()))
			}
		}
		delete(s.entries, prefix)
	case e.installed && slices.Equal(want, e.applied):
		s.entries[prefix] = e // nothing changed
	default:
		s.install(prefix, want, e, true)
	}
}

// install programs want for the prefix and records the result. With probe set,
// an unreachable next hop in an ECMP route is worked around by finding the
// reachable ones; without it (a retry of a route that is already partly
// installed) a refusal just leaves the route as it is.
func (s *Syncer) install(prefix netip.Prefix, want []netip.Addr, e entry, probe bool) {
	r := Route{Prefix: prefix, NextHops: want}
	before := 0
	if e.installed {
		before = len(e.applied)
		if e.live != nil {
			before = len(e.live)
		}
	}
	err := s.prog.Replace(r)
	ok := want
	var skipped []netip.Addr
	if err != nil && errors.Is(err, ErrNextHopUnreachable) && len(want) > 1 && probe {
		// One unreachable next hop must not take the reachable ones down with
		// it: program the ones that work.
		ok, skipped, err = s.installReachable(r)
	}
	if err != nil {
		if e.installed && !probe && errors.Is(err, ErrNextHopUnreachable) {
			s.entries[prefix] = e // still as partial as before; try again later
			return
		}
		s.noteFailure(prefix, r, err, e.failed)
		e.failed = true
		e.applied, e.live = nil, nil
		// Only an unreachable next hop can resolve by itself later.
		if errors.Is(err, ErrNextHopUnreachable) {
			s.pending[prefix] = struct{}{}
		} else {
			delete(s.pending, prefix)
		}
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
	s.recordInstall(prefix, e, want, ok, skipped, before)
}

// recordInstall is the bookkeeping after the kernel accepted a route: ok are
// the next hops that are in it and skipped the ones left out as unreachable.
func (s *Syncer) recordInstall(prefix netip.Prefix, e entry, want, ok, skipped []netip.Addr, before int) {
	if len(skipped) > 0 {
		s.stats.Unreachable += uint64(len(skipped))
		if !e.failed {
			s.logger.Warn("some next hops not reachable, route installed with the others",
				slog.String("prefix", prefix.String()), slog.Any("unreachable", skipped))
		}
		s.pending[prefix] = struct{}{}
		e.live = ok
	} else {
		delete(s.pending, prefix)
		e.live = nil
	}
	wasFailed := e.failed
	if !e.installed {
		s.nInstalled++
	}
	e.installed = true
	e.failed = len(skipped) > 0 // a partial route is not retried noisily
	e.applied = want
	s.stats.Replaced++
	if wasFailed && len(ok) > before {
		s.stats.Recovered++
		s.logger.Info("route installed after its next hop became reachable", slog.String("route", Route{Prefix: prefix, NextHops: ok}.String()))
	}
	s.entries[prefix] = e
	if s.debugLogs {
		s.logger.Debug("route installed", slog.String("route", Route{Prefix: prefix, NextHops: want}.String()))
	}
}

// installReachable is the fallback for an ECMP route the kernel refused
// because some next hop is unreachable. It finds the reachable next hops by
// trying each one alone, then installs those together. It returns the next
// hops that were left out. Only on the failure path does this cost extra
// kernel calls.
func (s *Syncer) installReachable(r Route) (ok, skipped []netip.Addr, err error) {
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
			return nil, nil, perr
		}
	}
	switch {
	case len(ok) == 0:
		return nil, nil, firstErr
	case len(ok) == 1:
		return ok, skipped, nil // the route the last probe left is the answer
	}
	if err := s.prog.Replace(Route{Prefix: r.Prefix, NextHops: ok}); err != nil {
		s.cleanProbe(r.Prefix, true)
		return nil, nil, err
	}
	return ok, skipped, nil
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
		// Counted and logged once per outage, not on every retry.
		if !already {
			s.stats.Unreachable++
			s.logger.Warn("route skipped, next hop not reachable, will retry when the network changes", slog.String("route", r.String()))
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
	dispIgnore      disposition = iota // not a path this package handles
	dispProgram                        // install this route
	dispRemove                         // remove our route for the prefix, if any
	dispNoInterface                    // link-local next hop and no interface to bind it to: remove, and warn
)

// classify decides what a best path event means for the kernel table.
//
//   - Only unicast IPv4 and IPv6 are handled; anything else is ignored.
//   - A withdrawal removes the route.
//   - A path with no peer is one this router originated (its next hop is
//     ourselves), a next hop GoBGP marked invalid, or a next hop of a different
//     address family than the prefix cannot be programmed; if such a path is
//     now the best, a route we installed earlier is stale, so it is removed.
//   - An IPv6 link-local next hop is bound to the interface named in the
//     peer's address (its zone); without one it cannot be programmed.
func classify(p *apiutil.Path) (netip.Prefix, netip.Addr, disposition) {
	if p == nil {
		return netip.Prefix{}, netip.Addr{}, dispIgnore
	}
	return classifyParts(p.Family, p.Nlri, p.Withdrawal, p.PeerAddress, p.IsNexthopInvalid, func() netip.Addr { return nextHopOf(p) })
}

// classifyChange is classify for the compact event of the server package.
func classifyChange(c *server.BestPathChange) (netip.Prefix, netip.Addr, disposition) {
	return classifyParts(c.Family, c.Nlri, c.Withdrawal, c.PeerAddress, c.IsNexthopInvalid, func() netip.Addr { return c.NextHop })
}

// classifyParts is the one place the rules above are written down. nextHop is
// only called for a path that could be programmed.
func classifyParts(family bgp.Family, nlri bgp.NLRI, withdrawal bool, peer netip.Addr, nhInvalid bool, nextHop func() netip.Addr) (netip.Prefix, netip.Addr, disposition) {
	var prefix netip.Prefix
	switch family {
	case bgp.RF_IPv4_UC, bgp.RF_IPv6_UC:
		n, ok := nlri.(*bgp.IPAddrPrefix)
		if !ok {
			return netip.Prefix{}, netip.Addr{}, dispIgnore
		}
		prefix = n.Prefix
	default:
		return netip.Prefix{}, netip.Addr{}, dispIgnore
	}
	if withdrawal || !peer.IsValid() || nhInvalid {
		return prefix, netip.Addr{}, dispRemove
	}
	nh := nextHop()
	if !nh.IsValid() || nh.IsUnspecified() || nh.Is4() != prefix.Addr().Is4() {
		// For example an IPv4 prefix with an IPv6 next hop needs a
		// different kernel encoding; not supported yet.
		return prefix, netip.Addr{}, dispRemove
	}
	if nh.Is6() && nh.IsLinkLocalUnicast() {
		// A link-local gateway means nothing without an interface (the kernel
		// refuses it), and the only place the interface is known is the
		// peer's address: GoBGP gives a peer reached over a link-local address
		// a zone.
		zone := peer.Zone()
		if zone == "" {
			return prefix, nh, dispNoInterface
		}
		return prefix, nh.WithZone(zone), dispProgram
	}
	return prefix, nh.WithZone(""), dispProgram
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

// ---- retry and drift repair (task E4-2) ----

// RetryPending tries again the prefixes that could not be fully installed
// because a next hop was not reachable. It is cheap when there are none, so it
// can run on every network change and on a timer.
func (s *Syncer) RetryPending() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	for prefix := range s.pending {
		e, ok := s.entries[prefix]
		if !ok {
			delete(s.pending, prefix)
			continue
		}
		want := s.wanted(prefix, e.cands)
		if len(want) == 0 {
			delete(s.pending, prefix)
			continue
		}
		s.install(prefix, want, e, !e.installed)
	}
}

// Verify checks one prefix against the operating system and puts our route
// back if it was removed or changed behind our back (an address removed or a
// link that went down makes the kernel drop routes through it). If the next
// hop is unreachable at that point the prefix becomes pending and comes back
// when the network does.
//
// Finding one route costs a pass over our routes in the kernel, so for many
// prefixes use VerifyAll.
func (s *Syncer) Verify(prefix netip.Prefix) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	e, ok := s.entries[prefix]
	if !ok || !e.installed {
		return
	}
	got, found, err := s.prog.Lookup(prefix)
	if err != nil {
		s.stats.Errors++
		s.logger.Error("failed to look up route", slog.String("prefix", prefix.String()), slog.String("error", err.Error()))
		return
	}
	s.checkDrift(prefix, e, got, found)
}

// VerifyAll is Verify for every route, with one pass over the kernel's routes.
func (s *Syncer) VerifyAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	owned, err := s.prog.ListOwned()
	if err != nil {
		s.stats.Errors++
		s.logger.Error("failed to list routes", slog.String("error", err.Error()))
		return
	}
	have := make(map[netip.Prefix]Route, len(owned))
	for _, r := range owned {
		if cur, dup := have[r.Prefix]; dup {
			r.NextHops = append(cur.NextHops, r.NextHops...)
			slices.SortFunc(r.NextHops, netip.Addr.Compare)
		}
		have[r.Prefix] = r
	}
	for prefix, e := range s.entries {
		if !e.installed {
			continue
		}
		got, found := have[prefix]
		s.checkDrift(prefix, e, got, found)
	}
}

func (s *Syncer) checkDrift(prefix netip.Prefix, e entry, got Route, found bool) {
	live := e.applied
	if e.live != nil {
		live = e.live
	}
	if found && slices.Equal(got.NextHops, live) {
		return
	}
	s.stats.Drifted++
	s.logger.Warn("kernel route was removed or changed outside veloced, restoring it",
		slog.String("prefix", prefix.String()), slog.Bool("found", found))
	e.installed = false
	e.applied, e.live = nil, nil
	s.nInstalled--
	want := s.wanted(prefix, e.cands)
	if len(want) == 0 {
		s.entries[prefix] = e
		return
	}
	s.install(prefix, want, e, true)
}

// changeBatch is what Run collects during the debounce time.
type changeBatch struct {
	prefixes map[netip.Prefix]struct{}
	reach    bool
	resync   bool
}

func (b *changeBatch) add(c Change) {
	if c.Prefix.IsValid() {
		if b.prefixes == nil {
			b.prefixes = map[netip.Prefix]struct{}{}
		}
		b.prefixes[c.Prefix] = struct{}{}
	}
	b.reach = b.reach || c.Reach
	b.resync = b.resync || c.Resync
}

// maxPrefixLookups is how many single-prefix checks one batch may make before
// the syncer does one full pass instead.
const maxPrefixLookups = 2

func (s *Syncer) process(b *changeBatch) {
	switch {
	case b.resync || len(b.prefixes) > maxPrefixLookups:
		s.VerifyAll()
	default:
		for p := range b.prefixes {
			s.Verify(p)
		}
	}
	// Whatever changed may also have made a waiting next hop reachable.
	s.RetryPending()
}

// Run watches the operating system for changes in the background until ctx is
// cancelled: it repairs routes the kernel dropped and retries prefixes whose
// next hop was not reachable, when something that could fix that changes and,
// as a safety net against lost notifications, every retryEvery (0 turns the
// timer off). It returns once the subscription is in place. Start it before
// Watch so no change between the two is missed.
func (s *Syncer) Run(ctx context.Context, src ChangeSource, retryEvery time.Duration) error {
	ch, err := src.Changes(ctx)
	if err != nil {
		return fmt.Errorf("watch network changes: %w", err)
	}
	go func() {
		var tick <-chan time.Time
		if retryEvery > 0 {
			t := time.NewTicker(retryEvery)
			defer t.Stop()
			tick = t.C
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick:
				s.RetryPending()
			case c, ok := <-ch:
				if !ok {
					return
				}
				var b changeBatch
				b.add(c)
				// Changes arrive in bursts (a link going down removes every
				// route through it); handle the burst once.
				timer := time.NewTimer(s.debounce)
			collect:
				for {
					select {
					case c2, ok := <-ch:
						if !ok {
							timer.Stop()
							return
						}
						b.add(c2)
					case <-timer.C:
						break collect
					case <-ctx.Done():
						timer.Stop()
						return
					}
				}
				s.process(&b)
			}
		}
	}()
	return nil
}
