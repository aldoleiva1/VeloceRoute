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

**Lab result: no measurable change** (r2, 900k routes, scratch n=6 vs decode n=3:
mallocs/route 140.0 vs 139.9, CPU 34.4 vs 33.2 s, converge 13.3 vs 12.9 s, all
within run spread). This is expected: `feedgen` puts one prefix in each UPDATE, and
with one prefix the slab saves nothing. The change should matter on real tables
where many prefixes share attributes; proving that needs the shared-attribute
mode for `feedgen` (not built yet). IPv6 (`MP_REACH_NLRI`) decode is not changed.

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

- The synthetic feed (`lab/tools/feedgen`) gives nearly every prefix its own AS
  path, so each UPDATE carries one prefix. Real tables share attributes across
  many prefixes, so the 40-prefix encode gain is closer to real behavior than the
  1-prefix lab result. A shared-attribute mode for `feedgen` is planned.
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
- Table-layer allocations (E1-3), `notifyBestWatcher` with no watchers (E1-4),
  `GOGC`/`GOMEMLIMIT` experiments (E1-5).
- Zero-copy `WatchEvent()` for local consumers, proto first (E1-6).
- Decoupling the Peer FSM loop (E1-7), the riskiest item, last.
