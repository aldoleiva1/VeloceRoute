# Performance work: what the code does and why

This page explains every change VeloceRoute has made on top of upstream GoBGP
v4.11.0 (`f7daa917`) for allocation and GC behavior, how each one works, how it
was verified, and what is not done yet. Task status lives in
[`ROADMAP.md`](../../ROADMAP.md) (section E1). Raw numbers and profiles live in
[`lab/results/`](../../lab/results/PROFILING-sync-pool.md).

## Why allocation matters here

A full-feed import means a router receiving ~900k routes in a burst. Profiling
r2 (the transit router in the lab) showed:

- `runtime.gcBgMarkWorker` took **57% of all CPU** during the import. The live
  heap holds the whole table, so every GC cycle rescans it.
- Cutting the *rate* of allocation cuts the number of GC cycles, and that is
  what saves CPU and convergence time. Cutting only bytes of live data would not.

So the goal is fewer short-lived allocations per route on the receive, process
and re-advertise path.

## Summary of changes

| # | Change | File | Effect |
|---|--------|------|--------|
| 1 | No per-UPDATE peer-config copy | `pkg/server/peer.go` | Removes ~27% of bytes allocated on the receive path |
| 2 | Debug-log arguments built only when debug is on | `pkg/server/peer.go` | Removes per-route string formatting on a transit router |
| 3 | Append-style NLRI writer, one buffer per UPDATE | `pkg/packet/bgp/bgp.go` | UPDATE encode allocations 26 to 17 (1 prefix), 184 to 17 (40 prefixes) |
| 4 | Exact-size attribute buffers, header and body in one buffer, pooled AS-path scratch | `pkg/packet/bgp/bgp.go` | Encode allocations 17 to 10 |
| 5 | Decode: one slab of prefix structs and one exactly-sized slice per UPDATE | `pkg/packet/bgp/bgp.go` | Decode allocations for 40 prefixes 69 to 24; no change for 1 prefix |
| 6 | Table layer: skip the best-path event with no watchers, lazy multipath lists, one-pass `PrependAsn`, bitmap by value, stack bitset in `GetPathAttrs` | `pkg/server/server.go`, `internal/pkg/table/` | Mallocs/route -24%, CPU -26% on the realistic feed |

Cumulative lab result, upstream vs changes 1 and 2 (r2, 900k routes, n=3 each,
interleaved runs, fresh daemons per run):

| metric | upstream | branch | change |
|---|---|---|---|
| mallocs/route | 193.8 | 162.2 | -16% |
| KB/route | 8.8 | 5.5 | -37% |
| GC cycles | 57.0 | 33.7 | -41% |
| CPU (s) | 70.1 | 52.8 | -25% |
| converge (s) | 23.5 | 17.8 | -24% |

Changes 3 and 4 on top (append vs scratch build, n=4 vs n=3): mallocs/route
154.2 to 139.9 (-9%, deterministic). CPU (-8%) and convergence (-7%) moved in
the right direction but the ranges overlap, so treat them as unproven.

## Realistic feed: 40 prefixes per attribute set

`feedgen -share 40` makes 40 consecutive prefixes carry one attribute set, so
GoBGP packs about 37 routes into each UPDATE (r2 received about 24,000 UPDATEs
for 900,000 routes, instead of 900,000). This is closer to a real table. Three
builds, interleaved, 3 runs each, r2, 900k routes
(`lab/results/share40-*`, `lab/results/variance-summary-share40.txt`):

| metric | upstream | server fixes only | all changes | all vs upstream |
|---|---|---|---|---|
| mallocs/route | 97.8 | 75.7 | 66.4 | **-32%** |
| KB/route | 3.7 | 2.9 | 2.8 | -26% |
| GC cycles | 28.7 | 25.7 | 24.7 | -14% |
| GC pause (ms) | 49.3 | 31.3 | 33.6 | -32% |
| CPU (s) | 19.9 | 15.2 | 14.2 | **-29%** |
| converge (s) | 9.4 | 7.5 | 7.3 | **-22%** |
| RSS (MB) | 1269 | 1115 | 1103 | -13% |

Reading it:

- The two server fixes give most of the gain (mallocs -23%, CPU -24%, convergence -20%).
- The packet-layer changes (encode and decode) add a further 12% fewer mallocs
  per route. The CPU (-7%) and convergence (-2%) improvements on top of the server
  fixes are inside the run-to-run spread (CPU ranges 13.4-16.9 s vs 13.7-15.2 s),
  so only the allocation count is a firm result there.
- All changes together vs upstream: CPU and convergence ranges do not overlap.
- Per-route cost is lower with shared attributes than with one prefix per UPDATE
  (66 vs 140 mallocs), because attribute work is amortized over about 37 prefixes.

## GOGC and GOMEMLIMIT experiments (E1-5)

No code change: the same build (all changes above) was run under different Go
runtime settings, passed to every gobgpd with `GOBGPD_ENV`. Feed: 900k routes,
`-share 40`. r2, mean of 3 per setting, interleaved
(`lab/results/gc*-run*.txt`, `lab/results/variance-summary-gc.txt`).

| setting | GC cycles | GC pause | CPU (s) | converge (s) | RSS (MB) |
|---|---|---|---|---|---|
| default (`GOGC=100`) | 24.7 | 36 ms | 15.3 | 7.5 | 1146 |
| `GOGC=200` | 12.0 | 16 ms | 12.6 (-18%) | 7.1 (-5%) | 1438 (+25%) |
| `GOGC=400` | 7.0 | 11 ms | 11.2 (-27%) | 7.4 (-1%) | 1632 (+42%) |
| `GOGC=off GOMEMLIMIT=2GiB` | 1.0 | 2 ms | 12.5 (-18%) | **8.8 (+17%)** | 2069 (+81%) |
| `GOGC=200 GOMEMLIMIT=1500MiB` | 12.0 | 18 ms | 13.0 (-15%) | 7.4 (-1%) | 1366 (+19%) |

Allocation counts per route are identical in every setting (66.4), as they should be:
these settings change how often the collector runs, not how much the code allocates.

What it shows:

- **Fewer GC cycles save CPU, with diminishing returns.** Doubling the GC target
  (`GOGC=200`) halves the cycles and saves 18% CPU for 25% more memory. Going to 400
  saves a further 9 points of CPU for another 17 points of memory and no better
  convergence time.
- **Turning GC off is worse.** With the collector nearly idle the heap grows to the
  2 GiB limit, and convergence got slower (8.8 s vs 7.5 s). Memory growth has its own
  cost (fresh pages from the OS), so "no GC" is not the fastest setting.
- **A memory limit works as a cap.** `GOGC=200` with `GOMEMLIMIT=1500MiB` stayed
  at 1366 MB, below the 1438 MB that `GOGC=200` alone used, for a small CPU cost.
  The limit is soft: Go collects more often as it approaches it.

**Recommendation** for a box with enough memory (the lab has no per-container limit):
`GOGC=200`, plus `GOMEMLIMIT` at about 75-80% of the memory available to the process
so a large table cannot push the host into swap or the OOM killer. Expect about 25%
more memory for 18% less CPU during a full-table import. Choose `GOGC=100` on small
instances. Not recommended: `GOGC=off`. These numbers are for one 900k-route import
into one router; steady-state memory under churn, IPv6 and many peers were not measured.
`veloced` now sets these defaults itself (see veloced.md); for `gobgpd`
set them in the environment:

```bash
# on a Linux router where gobgpd is installed on the PATH
GOGC=200 GOMEMLIMIT=6GiB gobgpd -f /etc/gobgp.toml

# from a repo checkout after `go build -o bin/gobgpd ./cmd/gobgpd`
GOGC=200 GOMEMLIMIT=6GiB ./bin/gobgpd -f <your-config>.toml

# in the lab, for every router (run from lab/)
GOBGPD_ENV="GOGC=200 GOMEMLIMIT=6GiB" ./ab.sh my-run gobgpd-decode
```

`gobgpd` listens on TCP 179, which needs root or `CAP_NET_BIND_SERVICE`, and the lab
configs use the lab's addresses, so they only establish sessions inside the lab.

## 6. Table layer (E1-3, E1-4)

Found by an allocation profile of r2 on the realistic feed (`lab/profile.sh`,
`lab/results/prof/tbl-base_*`). `propagateUpdate` runs once per path, so every small
allocation in it is paid once per route.

| Change | Where | What it does |
|---|---|---|
| Skip the best-path event with no watchers | `notifyBestWatcher` in `pkg/server/server.go` | The function cloned every best path, built a map and an event, then handed it to zero watchers. It now returns early when `isWatched(watchEventTypeBestPath)` is false. This was the largest single site (15% of sampled allocations). Behavior with a watcher is unchanged. |
| Optional multipath lists stay nil | `dstsToPaths` | Three of its five result slices are only filled when multiple paths are in use; they are now created on first append instead of for every call. Callers only range over them or take their length. |
| One-pass `PrependAsn` | `internal/pkg/table/path.go` | It deep-copied the whole AS_PATH, then rebuilt its first segment, then (when the first segment had a different type) copied the list again. It now builds the final segment list once and shares the segments that are already `As4PathParam` (segment contents are never modified after creation; the segment *list* is always a fresh slice because some code replaces entries in it). Non-4-octet segments are still converted to fresh `As4PathParam`s as before. The attribute length and flags are now computed from the final path, where before they kept the pre-prepend values. |
| Bitmap by value | `destination` | The per-destination add-path id bitmap is stored by value, saving one allocation per destination. |
| Stack bitset in `GetPathAttrs` | `internal/pkg/table/path.go` | A fixed array on the stack replaces a heap bitmap, and the `modified` map is created only for derived paths. This makes the call about 15% faster (26 to 22 ns); it does **not** reduce allocations, because Go already kept the old bitmap on the stack. |

**Verification.** `internal/pkg/table/path_alloc_test.go` keeps copies of the old
`PrependAsn`, `cloneAsPath` and `GetPathAttrs` and compares: 3000 random AS paths
(2- and 4-octet segments, all four segment types, full and nearly full segments,
repeat counts that overflow 255), byte for byte on the wire plus `String()`;
2000 random chains of derived paths for `GetPathAttrs`; and a test that the
original path is untouched after derived paths prepend. Three deliberate bugs (wrong
front count, ignored deletions, mutating a shared segment) were each caught. Existing
table and server tests pass; `pkg/server` shows only the four known macOS failures.
Five existing destination tests changed `NewBitmap(64)` to `makeBitmap(64)` for the new field type.

**Result** (r2, 900k routes, `-share 40`, n=3 each, interleaved, previous build vs this one;
`lab/results/variance-summary-table.txt`):

| metric | previous build | with table-layer changes | change |
|---|---|---|---|
| mallocs/route | 66.4 | 50.4 | **-24%** |
| KB/route | 2.8 | 2.3 | -18% |
| GC cycles | 24.0 | 21.3 | -11% |
| CPU (s) | 15.5 | 11.4 | **-26%** |
| converge (s) | 8.0 | 6.6 | **-18%** |
| RSS (MB) | 1095 | 1003 | -8% |

CPU and convergence ranges do not overlap. Sampled allocations per import fell from
41.8M to 27.1M (-35%). The sites gone or smaller in the new profile: `notifyBestWatcher` 6.5M to 0,
`NewBitmap` 2.8M to 0, `cloneAsPath` 3.0M to 0, `PrependAsn` 5.3M to 3.0M, `dstsToPaths` 4.2M to 2.8M.
There is no build per individual fix, so the gain is not split between them; the profile
suggests the best-path event skip is the largest part.

Against the upstream builds measured earlier the same day on the same feed (not interleaved with
this batch, so indicative only): mallocs/route 97.8 to 50.4, CPU 19.9 to 11.4 s, converge 9.4 to 6.6 s.

**Still allocating in the table layer** (new profile, share of sampled objects): `UpdatePathAttrs`
18%, `TableManager.Update` 17%, the update packer 12%, `getOrCreateDest` 11% (a prefix string key
per lookup and the destination), `PrependAsn` 11%, `dstsToPaths` 10% (best and old lists), `updateRoutes` 9%.

## 1. Per-UPDATE config copy (`pkg/server/peer.go`)

**What upstream did.** `handleUpdate` called `pConf.ReadCopy()` for every UPDATE
message, copying the entire peer configuration just to set
`Timers.State.UpdateRecvTime`.

**What the code does now.** Nothing is copied. The receive time is already
stored atomically by the FSM (`pkg/server/fsm.go`, where the UPDATE is read) and
read back when peer state is built (`pkg/server/server.go`). The copy was
redundant, so it was deleted.

## 2. Lazy debug logging (`pkg/server/peer.go`)

**What upstream did.** Calls like `logger.Debug("From me, ignore", slog.String(..., path.String()))`
evaluate their arguments before the logger decides to drop the message. On a
transit router every route is filtered back toward its source, so
`path.String()` and `slog.Any` boxing ran for every route even with debug off.

**What the code does now.** Each such call is wrapped in
`if peer.fsm.logger.Enabled(context.Background(), slog.LevelDebug) { ... }`.
Behavior with debug on is unchanged. Sites: "received update", "From me,
ignore", "Originator ID is mine, ignore", cluster-list loop, and "next hop is
our own address, ignore".

## 3. Append-style NLRI writer (`pkg/packet/bgp/bgp.go`)

**What upstream did.** Each prefix was serialized into its own small slice,
those slices were collected, then concatenated into the output. For an UPDATE
with 40 prefixes that is about 180 allocations.

**What the code does now.**

- `IPAddrPrefix.appendNLRI(dst)` writes the length octet and the prefix bytes
  straight onto `dst` (`appendPrefix` handles IPv4 and IPv6 via `netip`).
- `PathNLRI.appendTo(dst, isAddPath)` writes the optional 4-byte add-path ID,
  then uses `appendNLRI` if the NLRI type implements the unexported
  `nlriAppender` interface. Only plain IPv4/IPv6 prefixes do. Every other NLRI
  type (labeled, VPN, EVPN, flowspec, and so on) falls back to its existing
  `Serialize()` and is appended, so behavior for them is unchanged.
- `BGPUpdate` builds the whole body in a single pre-sized buffer: withdrawn
  length (2 octets), withdrawn routes, attribute length (2 octets), attributes,
  NLRI. The two length fields are patched in place after the data is written.
- `MP_REACH_NLRI` and `MP_UNREACH_NLRI` serialization uses `appendTo` as well.

## 4. Exact buffers, one-buffer message, pooled scratch

- **`PathAttribute.Serialize`** allocates once with the exact size (header plus
  value) instead of `make` followed by `append`.
- **`BGPMessage.Serialize` for UPDATE** goes through `serializeUpdate`: it
  reserves the 19-byte header at the front of the buffer, has
  `BGPUpdate.appendBody` write the body after it, then fills in the header with
  `BGPHeader.putHeader`. This removes the copy that prepended the header to a
  separately built body. The 4096 / 65535 (extended message) length checks are
  the same as before.
- **`PathAttributeAsPath.Serialize`** builds the concatenated AS segments in a
  buffer taken from `scratchPool` (a `sync.Pool`). `PathAttribute.Serialize`
  copies the value into its own exactly-sized result, so the scratch is
  returned to the pool immediately and nothing pooled escapes. Buffers larger
  than 64 KiB are dropped instead of pooled.
- **Cached length fields.** `BGPUpdate.WithdrawnRoutesLen` and
  `TotalPathAttributeLen` are written only if the value changed. A message
  received from a peer is shared by the FSM, the gRPC API and BMP clients, which
  serialize it from different goroutines, and it already carries the decoded
  lengths. Writing them every time was a data race that `go test -race` caught
  once the concurrency test existed.

### Rules for using `sync.Pool` in this codebase

A pool is only safe when the pooled object cannot be referenced after it is
returned. In practice:

- **Safe:** temporary buffers that are copied into a fresh result before the
  function returns (the AS-path scratch).
- **Not safe:** the serialized message bytes. The caller sends them and may keep
  them (adj-out, BMP, MRT), so they must be freshly allocated.
- **Not safe on decode:** decoded prefixes and attributes become part of the
  returned message and live in the RIB. The decode-side plan is batch
  allocation (one slab of structs per UPDATE), not pooling.

## 5. Decode: batch allocation of prefixes (`pkg/packet/bgp/bgp.go`)

**What upstream did.** `BGPUpdate.DecodeFromBytes` allocated one `IPAddrPrefix`
per withdrawn route and per NLRI prefix, and grew the `PathNLRI` slice with
`append`. The profile of a 40-prefix UPDATE put 59% of decode allocations on the
`&IPAddrPrefix{}` line and another 10% on the slice growth. The withdrawn-routes
slice was also created with a capacity equal to the withdrawn length in *bytes*.

**What the code does now.**

- `countIPv4NLRI` walks only the length octets of the withdrawn and NLRI fields
  and returns how many prefixes they appear to hold (add-path aware). It is a
  capacity hint, not validation: it stops at the first prefix that does not fit.
- `prefixSlab` allocates `[]IPAddrPrefix` once for that count, and `next()` hands
  out elements. If the hint was too small (malformed input) it falls back to a
  normal `&IPAddrPrefix{}`, so a wrong hint only costs an allocation.
- The `PathNLRI` slices are created with the exact capacity, so no regrowth.
- The decode loops are unchanged and still do all validation and produce the
  same errors. Empty fields remain non-nil empty slices as before.

**Why a slab and not `sync.Pool`.** Decoded prefixes become part of the message
and then the RIB, so they outlive the call and cannot be returned to a pool.
Batch allocation is the safe equivalent. Elements of a slab are distinct memory,
so there is no aliasing between prefixes. The trade-off: while any prefix of an
UPDATE is still referenced, the whole slab stays reachable. That is bounded by
one UPDATE (at most about 800 prefixes in a 4096-octet message) and in practice
all prefixes of an UPDATE live as long as each other.

**Verification.** `TestUpdateDecodeSlabMatchesLegacy` runs 4000 random withdrawn
and NLRI fields (add-path on and off, a third of them corrupted by byte flips,
truncation or trailing junk) and requires the same prefixes, ids and
success/failure as a copy of the old loop. `TestUpdateDecodeSlabNoAliasing`
checks that changing one decoded prefix leaves its neighbours alone and that no
two entries share a pointer. A deliberate bug (every element pointing at the
first) was caught by both tests.

**Result.** Decode, median of 8:

| case | before | after |
|---|---|---|
| 1 prefix | 530 ns, 664 B, 24 allocs | 530 ns, 664 B, 24 allocs |
| 40 prefixes | 1952 ns, 5320 B, 69 allocs | 1115 ns, 3040 B, 24 allocs |

**Lab result.** With one prefix per UPDATE there is no measurable change (scratch
n=6 vs decode n=3: mallocs/route 140.0 vs 139.9, CPU 34.4 vs 33.2 s, converge 13.3
vs 12.9 s, all within run spread), as expected since the slab saves nothing for a
single prefix. With about 37 prefixes per UPDATE (`-share 40`) the packet-layer
changes together cut mallocs/route by 12% (75.7 to 66.4); see the realistic-feed
section. IPv6 (`MP_REACH_NLRI`) decode is not changed.

## How it was verified

- **Byte-for-byte equivalence** (`pkg/packet/bgp/nlri_append_test.go`). The file
  keeps copies of the old implementations (`legacyToSlice`,
  `legacyUpdateSerialize`, `legacyMessageSerialize`) and compares the new output
  with random IPv4/IPv6 prefixes, add-path on and off, labeled and VPN NLRI,
  MP_REACH/MP_UNREACH, long AS paths that need extended-length attributes, and
  messages that cross the 4096 limit with and without the extended-message
  option.
- **Concurrency** (`TestSerializeConcurrentPool`): 8 goroutines serialize one
  shared message under `-race`.
- **Mutation testing:** two deliberate bugs (length octet +1, attribute length
  off by one) were introduced and both were caught by these tests.
- **Benchmarks** (`pkg/packet/bgp/update_bench_test.go`): `BenchmarkUpdateSerialize`
  and `BenchmarkUpdateDecode` with 1 and 40 prefixes. Run:

  ```bash
  go test -run XXX -bench 'UpdateSerialize|UpdateDecode' -benchmem -count=8 ./pkg/packet/bgp/
  ```

  Median of 8 (Apple silicon, Go benchmark):

  | case | upstream | after change 3 | after change 4 |
  |---|---|---|---|
  | encode 1 prefix | 475 ns, 600 B, 26 allocs | 349 ns, 472 B, 17 allocs | 262 ns, 344 B, 10 allocs |
  | encode 40 prefixes | 2764 ns, 2272 B, 184 allocs | 622 ns, 968 B, 17 allocs | 522 ns, 664 B, 10 allocs |
  | decode 1 / 40 prefixes | 24 / 69 allocs | unchanged | unchanged (24 / 24 after change 5) |

- **Known unrelated test failures on macOS:** `TestDisableMrt`, `TestEBGPRouteStuck`,
  `TestRTCDeferralTimerRaceCondition` and `TestTcpAoUpdatePeerWithClosedConnection`
  need a loopback alias (127.0.0.201) and TCP-AO. They fail the same way on
  pristine upstream. Roadmap task A-2 covers running them on Linux.

## How to reproduce the lab numbers

See [`lab/README.md`](../../lab/README.md) for deploying the three-router lab.
Then:

```bash
cd lab
# build two linux binaries (for example upstream and your branch) and compare:
./run-ab2.sh <labelA> <binA> <labelB> <binB> 3     # interleaved, fresh daemons each run
python3 summarize.py <labelA> <labelB>             # mean and spread table
```

`ab.sh` restarts gobgpd on all routers, waits for the old processes to exit,
waits for sessions, then runs `baseline.sh` (injects 900,000 /24s from r1 and
records r2/r3 runtime metrics from Prometheus on :6060).

## Limits of the measurements

- By default the synthetic feed (`lab/tools/feedgen`) gives every prefix its own
  AS path, so each UPDATE carries one prefix. Use `-share 40` for a feed where
  UPDATEs carry about 37 prefixes (see the section above). Real tables vary; the
  true mix of prefixes per UPDATE depends on the peer. The feed is IPv4 /24s only.
- The lab runs in Docker Desktop on one laptop with swap nearly full. CPU and
  convergence time are noisy (run-to-run spread of 10-15%); allocation counts
  per route are deterministic and are the reliable signal.
- GC pause and RSS differences are within noise.

## Not done yet

Tracked in [`ROADMAP.md`](../../ROADMAP.md):

- Decode: IPv6 (`MP_REACH_NLRI`) prefixes, the per-attribute allocations and the
  attribute-presence map are untouched (24 allocations per UPDATE remain). E1-2.
- Remaining 10 encode allocations: the per-UPDATE attribute-presence map, the
  options slice and small per-attribute buffers.
- Remaining table-layer sites listed under change 6 (E1-3 is partly done).
- (done in E2-5) `veloced` chooses `GOGC`/`GOMEMLIMIT` defaults from the instance memory.
- Zero-copy `WatchEvent()` for local consumers, proto first (E1-6).
- Decoupling the Peer FSM loop (E1-7), the riskiest item, last.
