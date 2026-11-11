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

## Variance runs: upstream vs branch (3 interleaved runs each, fresh daemons, 900k feed, r2)

Upstream = commit f7daa917. Branch = same plus the two `peer.go` fixes. Runs alternated upstream/branch; each started fresh `gobgpd` on all three routers. Raw files: `results/upstream-run*.txt`, `results/branch-run*.txt`; table: `results/variance-summary.txt`. Host was under swap pressure (about 22 GB of 24 GB), so timing metrics carry noise.

| r2 metric | upstream (mean [min..max]) | branch (mean [min..max]) | change | separated? |
|---|---|---|---|---|
| mallocs / route | 193.8 [193.3 .. 194.0] | 162.2 [162.1 .. 162.3] | -16% | yes (deterministic) |
| KB / route | 8.8 [8.8 .. 8.8] | 5.5 [5.5 .. 5.6] | -37% | yes (deterministic) |
| GC cycles | 57.0 [55 .. 58] | 33.7 [32 .. 35] | -41% | yes |
| CPU seconds | 70.1 [64.0 .. 76.7] | 52.8 [47.5 .. 55.5] | -25% | yes (ranges do not overlap) |
| converge r2+r3 | 23.5 s [21.7 .. 25.7] | 17.8 s [15.7 .. 19.0] | -24% | yes (ranges do not overlap) |
| GC pause total | 112 ms [103 .. 129] | 83 ms [68 .. 109] | -25% | no (ranges overlap) |
| peak RSS | 1682 MB [1623 .. 1741] | 1604 MB [1551 .. 1689] | -5% | no (ranges overlap) |

Reading: allocation counts are essentially deterministic (spread under 0.5%), so the -16% mallocs and -37% bytes per route are solid. GC cycles, CPU and convergence time are separated by the observed ranges with n=3. GC pause total and RSS are not distinguishable from noise. The earlier single-run claim that total GC pause rose (89 -> 154 ms) was noise.

Process notes: a first attempt failed because killed `gobgpd` processes linger as zombies (container PID 1 is `sleep`), which the wait loop counted; `ab.sh` now ignores zombies. Partial runs from before the fix are in `results/discarded/`.


## sync.Pool / allocation baseline for the packet layer (Apple M3 Pro, `-cpu=1`, 8 samples, upstream + peer.go fixes)

Command: `go test ./pkg/packet/bgp/ -run '^$' -bench 'BenchmarkUpdate(Serialize|Decode)' -benchmem -count=8 -cpu=1`. Raw output: `results/bench-baseline-encode-decode.txt`. Allocation numbers are exact and repeatable; ns/op varies about 10% (first sample of each case is a warm-up outlier).

| benchmark | median ns/op [min..max] | B/op | allocs/op |
|---|---|---|---|
| UpdateSerialize/nlri=1 (n=8) | 475 [468 .. 670] | 600 | 26 |
| UpdateSerialize/nlri=40 (n=8) | 2764 [2552 .. 3001] | 2272 | 184 |
| UpdateDecode/nlri=1 (n=8) | 592 [580 .. 666] | 664 | 24 |
| UpdateDecode/nlri=40 (n=8) | 2016 [2007 .. 2051] | 5320 | 69 |

Allocation sites (exact profile, `-memprofilerate=1`, 40-prefix UPDATE):

- **Encode:** `IPAddrPrefix.Serialize` 50% of objects, `PathNLRI.toSlice` 25%, `BGPUpdate.Serialize` own appends 11%, `PathAttribute.Serialize` 5%. About 4 extra allocations per prefix (184 allocs for 40 prefixes vs 26 for 1): each prefix builds its own small byte slice, `toSlice` copies it again, then `append` grows the output buffer.
- **Decode:** `BGPUpdate.DecodeFromBytes` is 83% of objects (about 1.4 per prefix); attribute decode is small.

Implication for the plan: `sync.Pool` suits only temporary buffers whose contents are copied out before return. The per-prefix encode slices are better removed outright with an append-style serializer that writes into one pre-sized buffer. Decode objects escape into the returned message, so they cannot be pooled; batch-allocating prefix structs is the safer lever.

## Step 1 of encode work: append-style NLRI writer (`pkg/packet/bgp/bgp.go`)

Change: `IPAddrPrefix.appendNLRI` / `IPAddrPrefixDefault.appendPrefix` and `PathNLRI.appendTo` write each prefix straight into the output buffer; `BGPUpdate.Serialize` builds the body in one pre-sized buffer with the two length fields patched in place; the MP_REACH and MP_UNREACH serializers use the same writer. NLRI types without the fast path fall back to `Serialize`. `Serialize` output is unchanged.

Correctness: `nlri_append_test.go` compares the new code byte for byte against a copy of the old implementation (every IPv4 and IPv6 prefix length, add-path on and off, labeled and VPN NLRI via the fallback, whole UPDATEs with withdrawn routes, MP_REACH and MP_UNREACH). Two deliberate bugs (length octet and attribute length off by one) were both caught by these tests. `go test ./pkg/packet/... ./internal/... ./pkg/config/... ./pkg/apiutil/...` pass; `go test -race ./pkg/server/...` shows only the same four macOS failures as untouched upstream.

Benchmarks (median of 8, `-cpu=1`, raw in `results/bench-after-append-nlri.txt`):

| benchmark | ns/op | B/op | allocs/op |
|---|---|---|---|
| UpdateSerialize nlri=1 | 475 -> 349 (-27%) | 600 -> 472 | 26 -> 17 |
| UpdateSerialize nlri=40 | 2764 -> 622 (-77%) | 2272 -> 968 | 184 -> 17 |
| UpdateDecode (both) | unchanged | unchanged | unchanged |

Lab, one run on r2 (n=1, indicative only): mallocs/route 162.2 -> 154.2, CPU 52.8 s -> 41.5 s, converge 17.8 s -> 15.4 s, KB/route unchanged (5.5 -> 5.4).
Why the lab gain is smaller than the benchmark: `feedgen` gives almost every prefix its own AS path, so each UPDATE carries one prefix, and the saving is the 9 allocations of the nlri=1 case (about 8 per route observed). Real internet tables pack many prefixes behind shared attributes, so the nlri=40 style gain should show up there. Consider adding a mode to `feedgen` that shares attributes across prefixes.

Remaining encode allocations (17 per UPDATE) are fixed cost: per-attribute buffers, the message header copy and the attribute list helper. Next candidates: pre-size `PathAttribute.Serialize` (2 allocs to 1), reserve header space so `BGPMessage.Serialize` does not copy the body, and pool the scratch buffers.
