# Profiling findings: where r2 allocates during a 900k full-feed import

Setup: upstream gobgpd v4.11.0 (commit f7daa917), r1 (AS65001) -> r2 (AS65002, transit, device under test) -> r3 (AS65003).
Feed: 900,000 deterministic /24s from `feedgen`, AS-path length 2-6, 0-3 communities. r2 profiled via pprof on :6060.
Profiles in `results/prof/` (`allocs_before` / `allocs_after` diffed with `-base`; `cpu.pb.gz` is 28 s during the injection).

## Headline

- **GC dominates CPU.** `runtime.gcBgMarkWorker` is **56.9%** of all CPU samples during the import; `mallocgc` is another 13.7%. The live heap holds ~900k routes, so every GC cycle rescans it. Cutting allocation *rate* cuts GC cycles, which is what costs.
- **Path-attribute serialization is not the biggest source.** The largest avoidable allocations are in the server layer, not in `pkg/packet/bgp`.

## Allocation sources, ranked (diff of the import only)

| # | Site | Evidence | Fix direction |
|---|------|----------|---------------|
| 1 | `peer.handleUpdate` calls `pConf.ReadCopy()` on **every UPDATE** just to set `Timers.State.UpdateRecvTime` (`pkg/server/peer.go`, ~line 850) | `handleUpdate` flat **1.31 GB (20% of bytes)**, `pConfAccess.ReadCopy` another **497 MB (7.6%)** | Store the receive time in an `atomic.Int64` on the peer/fsm and read it when building state. No config copy, no lock. |
| 2 | Debug log arguments built even when debug is off: `logger.Debug("received update", slog.Any(...)x3)` boxes three slices each UPDATE; `path.String()` is evaluated eagerly in `filterPathFromSourcePeer` ("From me, ignore") and similar sites | `Path.String` 12.2M objects (8.8%), `PeerInfo.String` 6.0M, `netip.Addr.string4` 3.8M, `bytes.growSlice` 2.3M. `filterPathFromSourcePeer` 11.1% cum. On a transit router every route is filtered back toward its source, so this runs per route | Guard with `logger.Enabled(ctx, slog.LevelDebug)`, or pass a lazy `slog.LogValuer`. Zero behaviour change. |
| 3 | Packet decode: `BGPUpdate.DecodeFromBytes` | 17.0M objects cum (12.3%), 424 MB | `sync.Pool` for decode scratch (attribute slices, NLRI slices). Must be released only after paths are built and no longer reference the buffers. |
| 4 | Packet encode: `BGPUpdate.Serialize` (9.3% objects), `PathAttribute.Serialize` (5.5%, flat), `PathAttributeAsPath.Serialize` (5.7% cum), `bytes.growSlice`, `NewBGPUpdateMessage` | 12.8M objects cum, 385 MB | This is the `sync.Pool` item in CLAUDE.md. Pool the output buffers (pre-sized) and the per-attribute temp slices. |
| 5 | Table layer: `table.NewBitmap` (3.2%), `Path.Clone` (2.4%, 304 MB), `clonePathList`, `dstsToPaths`, `Path.GetPathAttrs` | 4.4M + 3.3M + 4.9M objects | Lazy bitmap allocation; avoid cloning when the path is only read. |
| 6 | `notifyBestWatcher` | 8.0M objects cum (5.8%), 462 MB | Skip building watch events when there are no watchers. |

## Recommendation

1. Do items 1 and 2 first. They are small, local, behaviour-preserving, and together remove roughly **25-30% of bytes allocated** and a large share of per-route string formatting. They need no `sync.Pool`.
2. Then do the `sync.Pool` work (items 3 and 4) in `pkg/packet/bgp/`, measured with `go test -bench` plus this lab run.
3. In parallel, test the cheap runtime levers without code changes: `GOGC` (e.g. 200-400) and `GOMEMLIMIT`, since the cost is GC marking of a large live heap.

## Baseline to beat (r2, before-after counters for the 900k import)

194 mallocs/route, 8.8 KB/route, 51 GC cycles, 89 ms total pause, 81 s CPU, 26 s to converge r2+r3, ~1.76 GB RSS.

## Reproduce

    cd lab
    docker exec clab-gobgp3-r1 feedgen -n 900000 -withdraw      # clean table
    docker exec clab-gobgp3-r2 sh -c 'wget -qO /tmp/b.pb.gz 127.0.0.1:6060/debug/pprof/allocs; (wget -qO /tmp/cpu.pb.gz "127.0.0.1:6060/debug/pprof/profile?seconds=28" &)'
    docker exec clab-gobgp3-r1 feedgen -n 900000
    docker exec clab-gobgp3-r2 wget -qO /tmp/a.pb.gz 127.0.0.1:6060/debug/pprof/allocs
    # docker cp the files out, then:
    go tool pprof -sample_index=alloc_objects -base b.pb.gz -top a.pb.gz

## Result after fixes 1 and 2 (single run each, same 900k feed)

Change: `pkg/server/peer.go` only. Removed the per-UPDATE `pConf.ReadCopy()`/`Update()` (UpdateRecvTime is already stored atomically in `fsm.go` and read in `server.go`), and guarded the debug-log arguments with `logger.Enabled(ctx, slog.LevelDebug)`.

| r2 (transit) | baseline | fix 1+2 | change |
|---|---|---|---|
| mallocs / route | 193.9 | 162.2 | -16% |
| KB allocated / route | 8.8 | 5.6 | -36% |
| GC cycles | 51 | 34 | -33% |
| CPU seconds | 81.2 | 68.6 | -16% |
| converge r2+r3 | 26.1 s | 22.2 s | -15% |
| peak RSS | 1764 MB | 1546 MB | -12% |

r3 also improved (KB/route 6.1 -> 2.9). r1, which only originates the feed, did not (150 mallocs/route both runs), which is the expected control.
Caveat: one run per configuration; r1's CPU moved 40 s -> 52 s with no relevant change, so expect roughly +/-20% run-to-run noise on CPU and GC pause. Repeat 3x before quoting percentages externally.
Tests: `go test -race ./pkg/server/...` passes except 4 tests that also fail on untouched upstream on macOS (TestDisableMrt, TestEBGPRouteStuck, TestRTCDeferralTimerRaceCondition, TestTcpAoUpdatePeerWithClosedConnection: loopback alias 127.0.0.201 and TCP-AO are unavailable).
