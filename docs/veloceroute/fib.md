# Linux kernel route sync (FIB sync)

`veloced -fib-sync` copies BGP best paths into the Linux kernel routing table, so
the host actually forwards traffic along what BGP learned. GoBGP by itself only
computes routes. Roadmap task E4-1; see [ROADMAP.md](../../ROADMAP.md) section E4 for
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

- **One next hop per prefix (no ECMP).** If GoBGP is configured for multipath, events
  carry several paths for one prefix and the last one wins in the kernel. ECMP is task E4-3.
- **No next hop resolution** beyond what the kernel does (task E4-2). A next hop that is
  not on a connected network is skipped, not retried when a route to it appears later;
  it is installed when the next best path event for that prefix arrives.
- **No batching.** One netlink call per route; the 900k-route table has not been
  measured (task E4-5). The watcher queue is unbounded, so a slow kernel means memory
  grows, not that BGP slows down.
- IPv4 and IPv6 unicast only, and the next hop must be the same family as the prefix
  (an IPv4 prefix with an IPv6 next hop is skipped).
- Tested on the Docker Desktop Linux VM kernel (arm64). Not yet in CI (tasks A-6, E4-6).
- Routes are removed when `veloced` stops, so a restart briefly leaves the host
  without BGP routes. Graceful-restart style retention is not implemented.
