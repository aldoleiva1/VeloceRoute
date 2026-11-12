# Linux kernel route sync (FIB sync)

`veloced -fib-sync` copies BGP best paths into the Linux kernel routing table, so
the host actually forwards traffic along what BGP learned. GoBGP by itself only
computes routes. Roadmap tasks E4-1, E4-2 (next hop retry, recovery, link-local binding), E4-3 (ECMP) and E4-5 (bulk programming at 900k); see [ROADMAP.md](../../ROADMAP.md) section E4 for
what is still to come.

Source: [`internal/pkg/fib/`](../../internal/pkg/fib/), wired into
[`cmd/veloced/`](../../cmd/veloced/) as the `fib` stage.

## Use

```bash
veloced -asn 65002 -router-id 2.2.2.2 -fib-sync          # needs root or CAP_NET_ADMIN, Linux only
ip route show proto 201                                   # the routes veloced owns
```

| flag | default | meaning |
|---|---|---|
| `-fib-sync` | off | turn kernel route sync on |
| `-fib-protocol` | 201 | route protocol id stamped on our routes (5 to 255; 0 to 4 are the kernel's own) |
| `-fib-table` | 254 (main) | routing table to program |
| `-fib-metric` | 20 | route metric; must be at least 1 (see below) |
| `-multipath` | off | keep every equal-cost BGP path, so the kernel route gets all their next hops (see ECMP) |
| `-fib-max-paths` | 64 | most next hops in one ECMP route (used with `-multipath`) |
| `-fib-stats-interval` | off | log the sync counters this often when they change, for example `10s` |
| `-pprof-address` | off | serve Go profiling endpoints (bind it to localhost) |
| `-fib-retry-interval` | 30s | safety-net timer for retrying routes whose next hop was unreachable (0 turns it off; network changes always trigger a retry) |

Peers are not configurable from a file yet (task E2-2); add them with the `gobgp`
CLI against the daemon's gRPC API, for example `gobgp neighbor add 10.0.12.1 as 65001`.

## How it works

1. **Start** (after the `bgp` stage). `veloced` checks it has `CAP_NET_ADMIN`
   and fails with one clear message if not. It opens a netlink handle, then **removes
   every route that carries our protocol, table and metric**. At that point no session
   is up, so anything left from an earlier run (for example after a crash) is stale.
2. **Watch.** It subscribes to GoBGP's best path events in process
   (`BgpServer.WatchEvent` with `WatchBestPath(true)`). This is the native Go callback,
   with no protobuf marshalling, and the first event is the current set of best paths.
   The callback runs on the watcher's own goroutine, one event at a time, so the syncer
   is the single writer of the kernel table, and BGP's own loop never waits for netlink.
3. **Each best path** is classified:
   - a learned unicast IPv4 or IPv6 path with a usable next hop: install it
     (`RouteReplace`), or replace it if the next hop changed;
   - a withdrawal: remove our route for the prefix, if we installed one;
   - a path that cannot be programmed: remove our route for that prefix, because the
     route we installed earlier no longer matches the best path. These are a locally
     originated path (no peer, so its next hop is ourselves), a next hop GoBGP marked
     invalid, and a next hop of the wrong address family for the prefix;
   - anything else (other address families, labeled or VPN prefixes): ignored.
4. **Stop** (before `bgp` stops). It removes every route it installed and ignores
   events still in flight, so a late event cannot put a route back after cleanup.

## Bulk programming (E4-5)

Measured on the lab (r2 with `veloced -fib-sync`, 900,000 /24s from r1 with `-share 40`,
Docker Desktop arm64 VM, three runs each, interleaved; `lab/fib-bulk.sh`,
`lab/summarize-bulk.sh`, results in `lab/results/bulk*-r*.txt`):

| | BGP converged | kernel table complete | veloced CPU | peak memory | SIGTERM removes all |
|---|---|---|---|---|---|
| one netlink call per route (before) | 6.3-6.7 s | 8.4-9.1 s (2.1-2.6 s after BGP) | 17.3-19.2 s | 1.29-1.41 GB | 6.7-7.2 s |
| batched (now) | 6.4-6.5 s | **same moment as BGP** | **11.0-12.7 s** | **0.98-1.03 GB** | **2.6-3.1 s** |
| batched, `GOGC=200` | 6.2-6.8 s | same moment as BGP | 8.3-10.0 s | 1.08-1.10 GB | 2.6-3.2 s |
| control: no kernel sync | 5.5-5.8 s | n/a | 4.1-5.2 s | 0.55-0.57 GB | 2.1-2.2 s |

(The kernel is polled every 0.5 s, so "same moment" means within that.) Kernel sync of the
whole table is no longer a tail after BGP converges, CPU is down by a third and memory by
a quarter. `GOGC=200` is a further third off CPU for 100 MB; `veloced` now uses it by
default (see [veloced.md](veloced.md#go-garbage-collector-defaults)), and a run with no
environment settings gave 8.9-10.1 s CPU and 1.13-1.32 GB, the same as the `GOGC=200` row.

**How.**
- **Group-commit queue.** The BGP watcher callback (`Watch`) only appends a compact copy of
  each best path event (about 90 bytes, not the multi-KB path) to a queue and returns;
  a writer goroutine takes everything that has queued up while it was busy and applies it
  as one batch. A slower kernel means bigger batches, not a stalled BGP. Order is kept, and
  several events for one prefix collapse into one change. The queue is unbounded, as before.
- **One system call for many routes.** A `BatchProgrammer` applies a list of changes.
  The Linux one builds RTM_NEWROUTE / RTM_DELROUTE messages straight into one reused buffer
  (no per-route objects) and sends up to 1024 messages or 64 KiB per `sendto`. Only the
  last message of a buffer asks for an acknowledgement: the kernel always answers a message
  that fails, and it handles messages in order, so the final acknowledgement means all
  earlier ones are done, and the errors that came before it name the ones that failed (by
  sequence number). Results mean the same as `Replace` and `Delete` (a delete of a route
  that is not there is fine; an unreachable gateway is `ErrNextHopUnreachable`).
- **A refused change takes the old path.** Anything the kernel refused inside a batch is
  repeated through the one-at-a-time code, which already knows what to do with each kind of
  refusal (wait for the next hop, probe an ECMP set, log). So the batch is only a fast path
  for the common case, and a test applies random events with and without batching and
  checks that the table and every counter come out the same.
- **Start and stop are batched too.** Removing stale routes at start and all routes at
  stop go through the same call: stopping with 900k routes took 6.7-7.2 s, now 2.6-3.1 s
  (2.1 s of that is GoBGP shutting down with a full table).
- Per-route debug logging no longer formats anything when debug is off.

**Where the time goes now** (CPU profile of one batched import, 12.1 s of samples,
`lab/results/prof/fibbatch_*`; `-pprof-address` makes this repeatable): garbage collection
is 54% of the CPU. The syncer's own work (`drain`) is 16%, and only 0.9 s of it is the
kernel call (`Apply`), mostly kernel time inserting routes. In that profile (before
E1-6), preparing events for the watcher in the BGP loop (`notifyBestWatcher`, cloning paths)
cost 0.44 s, and turning them into API paths in the watcher goroutine 0.31 s. Both are gone
with the compact watch events of E1-6: see the next section.
Live memory after the import: our per-prefix state is 168 MB (about 190 bytes per prefix:
the map slot plus a candidate and the applied next hops); the rest of the 0.43 GB over the
control is garbage waiting for the collector.

**Compact watch events (E1-6).** `Watch` now uses `server.WatchBestPathChanges` instead of
`WatchBestPath`. The old event carried clones of the paths (and the watcher goroutine then
built an attribute list and an `apiutil.Path` for each one), although the syncer only
needs the prefix, the next hop, the peer and whether the path was withdrawn. The new event
is a slice of `server.BestPathChange` values with just those fields, filled in the BGP loop
straight from the paths (so nothing is shared that could change meanwhile). With only
compact watchers there are no path clones, no vrf map and no attribute lists; gRPC
watchers and BMP use the old event as before, and if both kinds exist both are built.
Per event in the BGP loop (`BenchmarkNotifyBestWatcher`, one path): 7 allocations and
469 B before, 2 allocations and 138 B now. At 900k routes (`-share 40`, n=3, interleaved,
`lab/results/zc-*`):

| | BGP converged | kernel table complete | veloced CPU | peak memory |
|---|---|---|---|---|
| before (full paths) | 6.35-6.56 s | same moment as BGP | 8.2-9.2 s | 1.07-1.14 GB |
| compact events | 6.31-6.37 s | same moment as BGP | 7.4-7.8 s | 1.00-1.01 GB |
| control: no kernel sync | 5.87-6.24 s | n/a | 3.5-3.9 s | 0.56-0.57 GB |

CPU is 13% lower and memory 9% lower. BGP now converges 0.1-0.5 s after the control
instead of about 0.7 s (the ranges overlap: this laptop varies by that much from run to
run, so treat the remaining gap as small, not zero). The rest of the gap to the control is
the syncer's own work and the extra garbage it makes.

**The kernel side alone at 900k** (`FIB_SCALE_N=900000 /fib.test -test.run TestKernelScale`,
real kernel, events of 40 paths): install 1.9 s (22,500 batches), list all our routes
1.0 s, `VerifyAll` 1.2 s, remove all 0.6 s. So the kernel is not the bottleneck, and the
check after a link or address change (E4-2) costs about 1.2 s at 900k. It runs once per
burst and holds the syncer's lock; events queue meanwhile.

## Next hops that are not reachable, and routes the kernel drops (E4-2)

The kernel only accepts a route whose gateway is on a connected network, and it
removes routes behind our back. Both were found by probing a real kernel, not assumed:

- **A gateway that is not reachable yet** is refused (`ENETUNREACH`). Before E4-2 the
  route stayed missing until the next best path event for that prefix.
- **When an address is removed or a link goes down, the kernel deletes our routes
  through it, sends no notification for them, and does not bring them back** when the
  address or link returns. (It notifies only for the connected, local and broadcast
  routes.) Before E4-2 the daemon believed those routes were still installed.
- **A link-local IPv6 gateway with no interface is refused** ("Egress device not
  specified"), so IPv6 sessions over link-local addresses could not work at all.

What `veloced` does about it:

- **Retry.** A prefix whose next hop is unreachable is kept as *pending*, and tried again
  when something that could fix it changes (a link comes up, an address or a route is
  added), and on a timer (`-fib-retry-interval`) in case a notification was lost. The
  warning and the `Unreachable` counter are once per outage, not per retry. A withdrawal
  cancels the wait. Only "unreachable" is retried; other kernel errors will not fix
  themselves. A partial ECMP route that is retried is replaced as a whole in one call,
  and a refusal leaves it as it is, so a retry never flaps a working route.
- **Recovery.** The syncer subscribes to netlink route, address and link changes of the
  namespace it programs (`Syncer.Run`, started before the best path watch so nothing in
  between is missed). Because the kernel drops routes silently, an address or link
  change, or the removal of a kernel connected route, makes the syncer compare its
  routes with the kernel's in **one pass** over our routes (`VerifyAll`), put back any
  that are missing or changed (`Stats.Drifted`), and retry pending ones. If the next hop
  is unreachable at that moment the prefix becomes pending and returns with the network.
  Events are debounced (200 ms), so a link going down is handled once, not once per
  route. If the notification buffer overflows, the syncer is told to check everything.
  Installing our own routes does not cause any of this: only the removal of one of ours,
  and foreign changes, are reported.
- **Interface binding.** A link-local next hop is bound to the interface named in the
  peer's address. GoBGP gives a peer reached over a link-local address a zone
  (`fe80::1%eth1`); the syncer carries it on the next hop, and the Linux programmer turns
  it into an interface index per next hop (so ECMP over two interfaces works), cached and
  cleared on link changes. A zone can be a name or a number. A link-local next hop from a
  peer with no zone is not programmed; the operator is warned once per peer. An interface
  that does not exist is "unreachable", so the route is installed when it appears.

**Not done: recursive resolution.** A next hop that is not on a connected network (for
example an iBGP next hop several hops away) is not resolved through other routes; the
kernel refuses it and it waits as pending. Zebra-style resolution (look the next hop up in
the routing table and install the resolved gateway and interface, re-resolve when
routes change) is a separate piece of work. eBGP over WireGuard or an ENI, the target
deployments, has directly connected next hops.

## ECMP (multiple equal-cost paths)

With `-multipath`, `veloced` starts GoBGP with `use-multiple-paths`, and a prefix learned
from several peers over equal-cost paths becomes **one kernel route with all their next
hops** (`ip route` shows `nexthop via ...` lines, and the kernel spreads flows over
them). Without `-multipath` nothing changes: one best path, one next hop.

How it works, and why it is not just "add the next hops":

- **The event stream changes meaning.** Without multipath, an event says "this is the
  new best path", so a path from another peer *replaces* the old one. With multipath, GoBGP
  reports each equal-cost path as its own change: an update for a path that joined or
  changed, a withdrawal for a path that left (this includes every path of a peer whose
  session went down). The syncer is told which mode it is in (`fib.WithMultipath`) and
  keeps, per prefix, the eligible paths by source (peer address plus add-path id). The
  kernel route is rebuilt from that set: sorted, with duplicates removed, so the same
  paths always give the same route whatever order they arrived in.
- **One kernel change per prefix per event.** All paths of a prefix in one event are
  applied together, so the kernel never sees a half-built set and a 3-path event is one
  `RouteReplace`, not three.
- **The whole set is replaced at once** (`RTA_MULTIPATH`), IPv4 and IPv6, so traffic
  is never forwarded over a half-updated route. Repeating an event changes nothing.
- **An unreachable next hop does not take the others down.** The kernel refuses a
  multipath route if any next hop is unreachable. On that failure the syncer tries each
  next hop alone to find the reachable ones, installs those together, logs one warning
  naming the ones left out, and counts them in `Stats.Unreachable`. This costs extra
  kernel calls only on that failure path. If none is reachable, no route is installed
  and the old one is removed.
- **A cap** (`-fib-max-paths`, default 64) keeps the lowest next hop addresses and
  logs a warning, so the choice does not change between events.
- Ownership is unchanged: the same protocol, table and metric, deleted by prefix,
  so our ECMP route and someone else's route for the same prefix coexist and deleting
  ours leaves theirs (tested).

Not done: **weights** (unequal-cost / UCMP) and next hop groups (`nhid`). Every next
hop has weight 1.

## Safety: it only ever touches its own routes

Deleting a route must never remove one that a person, the kernel or another daemon
installed. A route is ours only if **protocol, table and metric** all match
(`proto 201`, table 254, `metric 20` by default).

The metric matters more than it looks. The kernel treats a "replace" request as
"replace the existing route with the same prefix, TOS and metric", whatever its
protocol. With metric 0, a BGP route for `10.0.12.0/24` would overwrite the kernel's own
connected route for that network. Our routes therefore always use a non-zero metric.
They coexist with connected and static routes (metric 0), which win on preference.
The live test shows this: `ip route show 10.0.12.0/30` lists the kernel's connected
route and ours side by side. The kernel test removes the metric on purpose and fails
with "our route and the static route must coexist, got [one route]", which is the
overwrite this prevents.

The one case that can still collide: another route with the same prefix, table, protocol
id **and** metric as ours. Pick a protocol id and metric nothing else uses.

## Verification

**Portable tests** (`go test -race ./internal/pkg/fib ./cmd/veloced`, any OS):
- install, replace on next hop change, no-op on an unchanged best path, withdraw,
  withdraw of an unknown prefix (no kernel call);
- IPv6 via MP_REACH and a link-local-only next hop;
- skipped: locally originated, wrong family, next hop invalid or unspecified, no next
  hop, VPN family, non-plain prefix; a local best path removes an earlier peer route;
- unreachable next hop: skipped, counted, a later withdraw makes no delete call,
  recovers when the next hop becomes reachable; a failed replace removes the old route;
  a failed delete is retried; `RemoveAll` continues after an error; stale-route
  reconcile; events after `RemoveAll` are ignored;
- **two real BGP servers on loopback**: one originates a prefix, the other learns it,
  the syncer on the second programs it and then removes it on withdraw, and the second
  server's own originated prefix is never programmed. This confirms a locally
  originated path really has no peer address in the real event stream;
- the `veloced` stage: starts after bgp and stops before it, removes stale routes on
  start and closes the programmer on stop, refuses to start without the privilege and
  leaves nothing running.

Three deliberate bugs (withdrawals ignored, local paths programmed, delete of
untracked prefixes) were each caught by these tests.

**Kernel tests** (Linux, `internal/pkg/fib/kernel_linux_test.go`): each test creates a
throwaway network namespace with a veth pair, so the host's routes are never touched.
They need `CAP_NET_ADMIN` and skip without it. Run them in a privileged container:

```bash
GOOS=linux GOARCH=arm64 go test -c -o /tmp/fib.test ./internal/pkg/fib      # GOARCH=amd64 on x86
docker run --rm --privileged -v /tmp/fib.test:/fib.test:ro gobgp-lab:local /fib.test -test.v
```

They cover install, replace and delete (stamped with our protocol and metric), foreign
routes for the same prefix and the connected route left alone, an unreachable next hop
reported as `ErrNextHopUnreachable`, the default route, IPv6, option validation, and
the whole syncer on a real kernel with stale-route reconcile. All pass; unprivileged,
the namespace tests skip and the rest pass.

ECMP adds kernel tests for IPv4 and IPv6 (install two next hops, grow to three, change
the set, shrink to a single route, grow again from a single route, delete leaves
nothing behind, `ListOwned` reports one route with sorted next hops), an unreachable
next hop in a multipath route leaving the old route untouched, a static route for the
same prefix surviving, and the syncer on a real kernel including the unreachable
fallback. The IPv6 case matters because the kernel handles IPv6 multipath routes
differently from IPv4; it passes on the Docker Desktop kernel.

`TestWatchMultipathBuildsAndShrinksEcmpRoute` runs three real BGP servers on loopback
(127.0.0.1 and 127.0.0.2 as peer addresses, so it is skipped on macOS): the syncer must
build the ECMP route from two peers, shrink it on an explicit withdrawal, grow it
again, and shrink it when one session drops. I checked the tests catch bugs by
planting three: treating multipath events as best paths, applying per path instead of
per event, and not removing duplicate next hops. Each made tests fail.

E4-2 adds portable tests for retry (counted once per outage, not repeated while still
unreachable, other failures not retried, a withdrawal cancels a retry, a partial route is
retried without probing), drift repair (dropped, changed, next hop gone then back, one
pass for many routes, nothing after shutdown), `Run` (restore on change, retry on a
reachability change, retry on the timer, a burst handled with one pass, resync) and
link-local binding. Kernel tests cover the link-local gateway being refused without an
interface and bound with one (by name and by number), ECMP over two interfaces,
`Lookup`, the notifications (our installs are silent; removal of ours, a foreign route,
an address and a link up are reported), and the three recovery cases on a real kernel:
address removed and re-added, link flap with an ECMP route, and a next hop whose network
appears later. I planted five bugs (probing on every retry, ignoring changed next hops,
counting every retry, never pending, zone not applied) and each made tests fail.

E4-5 adds portable tests (a fake batch programmer): many changes go in one batch,
unchanged ones are not sent, one change is not worth a batch, a refusal inside a batch
is handled as usual (also a partial ECMP route), a failed delete stays tracked,
Reconcile and RemoveAll batch, the random with-and-without-batching comparison, and the queue
(order kept, big batches under a slow kernel, stops with its context, nothing applied after
RemoveAll). Kernel tests compare a batch with single calls in two namespaces, down to
scope, type, protocol, metric and each ECMP next hop, over IPv4, IPv6, link-local with an
interface, replace, delete, deleting a missing route and refusals first, in the middle
and last; 5,000 routes across several buffers with refusals attributed to the right
operation; a batch mixed with single calls and a foreign route; and the syncer using a
batch on a real kernel. I planted two bugs in the encoder (an error attributed to the
wrong operation, routes not stamped with our protocol) and both made tests fail.

**Live E4-2 test in the lab** (`lab/retry-demo.sh`, output in `lab/results/retry-live-test.txt`,
16 checks, all pass): a prefix whose next hop network r2 does not have yet is not
installed and is logged once; adding the network installs it; removing and re-adding the
address and a link flap each drop and restore it; a prefix withdrawn while waiting is not
installed later; SIGTERM leaves nothing. Then the r1-r2 session is replaced by an
unnumbered one (`gobgp neighbor add interface eth1`) and an IPv6 prefix is installed
`via fe80::... dev eth1`, which confirms against real GoBGP that the peer address carries
the interface. (The `gobgp` CLI needs the peer in the kernel neighbor table first, so
the script pings `ff02::1` on both ends.)

**Live ECMP test in the lab** (`lab/ecmp-demo.sh`, output in `lab/results/ecmp-live-test.txt`,
15 checks, all pass): r1 and r3 both advertise the same prefixes to r2, which runs
`veloced -fib-sync -multipath`. A shared prefix is one kernel route with both next hops
(`proto 201 metric 20`); prefixes from one router have one; a withdrawal and a session
going down each shrink the route, and re-advertising and the session returning grow it;
SIGTERM removes it; the same paths without `-multipath` give a single next hop;
after `kill -9` the stale ECMP route is removed on the next start.

**Live test in the lab** (`lab/fib-demo.sh`, output in `lab/results/fib-live-test.txt`,
11 checks, all pass): `veloced -fib-sync` replaces `gobgpd` on r2 and peers with r1
and r3. Learned prefixes appear in `ip route show proto 201`; a prefix overlapping r2's
connected network leaves the kernel route intact; a prefix with an unreachable next
hop is skipped with one warning; withdrawals remove routes; SIGTERM removes all of
them; after `kill -9` the stale routes are removed on the next start, before any
session is up; and the kernel's own routes survive everything.

The live test found a bug the unit tests could not: with no namespace given the handle
was opened for namespace "0", which is a file descriptor and not "none". The tests
had always passed an explicit namespace. It is fixed (the option is now a pointer, nil
meaning the current namespace) with a regression test of the default path.

## Known limits

- **ECMP needs `-multipath`.** If GoBGP has multiple paths enabled by other means (the
  gRPC API after start) but `veloced` was not given `-multipath`, the syncer would read
  per-path events as best paths and the last path would win. Use the flag.
- **ECMP has no weights**, and uses one route per prefix with inline next hops, not
  kernel next hop objects (`nhid`). ECMP at 900k prefixes has not been measured; the
  numbers above are single-path.
- **No recursive next hop resolution** (see above): next hops must be on a connected network.
- **Recovery looks at all our routes.** Finding one route in the kernel means listing them
  (the kernel cannot fetch a route by exact prefix), so a link or address change costs
  one pass over our routes: 1.2 s at 900k (see Bulk programming). Changes are debounced,
  so a burst is one pass.
- Retry and recovery need the netlink subscription. If it cannot be set up, the daemon
  refuses to start.
- **The event queue is unbounded**, so a slow kernel means memory grows (about 90 bytes per
  waiting event), not that BGP slows down.
- **Per-prefix memory** is about 190 bytes (168 MB at 900k). It could be cut by interning
  addresses and storing the common single-path case inline; not done.
- **Batching is Linux only** (there is no other programmer). `Apply` serializes with
  itself; if the batch socket fails it is dropped and reopened on the next call.
- IPv4 and IPv6 unicast only, and the next hop must be the same family as the prefix
  (an IPv4 prefix with an IPv6 next hop is skipped).
- Tested on the Docker Desktop Linux VM kernel (arm64). Not yet in CI (tasks A-6, E4-6).
- Routes are removed when `veloced` stops, so a restart briefly leaves the host
  without BGP routes. Graceful-restart style retention is not implemented.
