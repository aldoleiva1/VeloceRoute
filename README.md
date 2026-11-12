# VeloceRoute
VeloceRoute is a lightweight software router for AWS VPC environments. It embeds a GoBGP control plane and WireGuard engine into a unified Go runtime for dynamic routing over secure tunnels.

Features:
- Single static binary
- gRPC API & OpenConfig support
- Seamless Amazon VPC Route Server peering
- Linux Netlink kernel table sync

## Status

Early stage. The repository starts from upstream [osrg/gobgp](https://github.com/osrg/gobgp) v4.11.0 (commit `f7daa917`, remote `upstream`) and is being tuned for allocation and GC behavior under full-table imports before the new features are added. The original GoBGP README is kept in [README-GoBGP.md](README-GoBGP.md).

**Not built yet:** the WireGuard engine and the Amazon VPC Route Server mode. Linux kernel route sync exists in a first form (`veloced -fib-sync`, one next hop per prefix, no ECMP; [docs/veloceroute/fib.md](docs/veloceroute/fib.md)). `veloced`, the single binary they will live in, exists as a skeleton ([docs/veloceroute/veloced.md](docs/veloceroute/veloced.md)): it runs the GoBGP engine and its gRPC API with ordered start and stop. Otherwise the code is upstream GoBGP plus performance work. The four features above are the target, and each is tracked as tasks in [ROADMAP.md](ROADMAP.md).

### Changes on top of upstream so far

All performance work; details, code walk-through and verification are in [docs/veloceroute/performance.md](docs/veloceroute/performance.md).

| Change | Where | Result |
|---|---|---|
| Removed a per-UPDATE peer-config copy; debug-log arguments built only when debug is on | `pkg/server/peer.go` | 37% fewer bytes and 41% fewer GC cycles per route on a transit router importing 900k routes (n=3) |
| Append-style NLRI writer, one buffer per UPDATE | `pkg/packet/bgp/bgp.go` | UPDATE encode allocations 26 to 17 (1 prefix), 184 to 17 (40 prefixes) |
| Exact-size attribute buffers, header and body in one buffer, pooled AS-path scratch | `pkg/packet/bgp/bgp.go` | Encode allocations 17 to 10; 9% fewer mallocs per route in the lab |
| Decode: one slab of prefix structs per UPDATE | `pkg/packet/bgp/bgp.go` | 40-prefix UPDATE decode 69 to 24 allocations |
| Table layer: no best-path event with no watchers, one-pass AS-path prepend, fewer small allocations per route | `pkg/server/server.go`, `internal/pkg/table/` | A further 24% fewer mallocs and 26% less CPU per route on the realistic feed |

On a feed where about 37 prefixes share each UPDATE (r2, 900k routes, n=3 each), all changes together vs upstream: 32% fewer mallocs per route, 29% less CPU, 22% faster convergence. Details in [docs/veloceroute/performance.md](docs/veloceroute/performance.md).

Runtime tuning (no code change): `GOGC=200` plus a `GOMEMLIMIT` cut CPU a further 18% during the same import for about 25% more memory; see the GOGC section of that document.

## Where the work is tracked

- **[ROADMAP.md](ROADMAP.md)** is the task list for getting to a finished product: every task has an ID, dependencies and a "done when" check. It starts with a progress table showing what is done, in progress and not started.
- [docs/veloceroute/performance.md](docs/veloceroute/performance.md) explains the performance changes.
- [docs/veloceroute/fib.md](docs/veloceroute/fib.md) describes the kernel route sync: how it works, why it only touches its own routes, how it was tested, limits.
- [docs/veloceroute/veloced.md](docs/veloceroute/veloced.md) describes the `veloced` daemon: flags, lifecycle, how stages are added, limits.
- [docs/veloceroute/blueprint.md](docs/veloceroute/blueprint.md) is the corrected design for the WireGuard, Netlink and AWS Route Server work, with the AWS facts checked against AWS documentation.

## Building and testing

```bash
go build -v -o bin/gobgpd ./cmd/gobgpd && go build -v -o bin/gobgp ./cmd/gobgp
CGO_ENABLED=0 go build -o bin/veloced ./cmd/veloced     # the VeloceRoute daemon (static)
go test -v ./pkg/packet/bgp/...        # packet tests
go test -race ./cmd/veloced ./internal/pkg/fib   # veloced and kernel route sync (the netlink tests skip without CAP_NET_ADMIN)
go test -v -race ./pkg/server/...      # server tests (4 fail on macOS, see below)
go test -run XXX -bench 'UpdateSerialize|UpdateDecode' -benchmem ./pkg/packet/bgp/
```

Four `pkg/server` tests (`TestDisableMrt`, `TestEBGPRouteStuck`, `TestRTCDeferralTimerRaceCondition`, `TestTcpAoUpdatePeerWithClosedConnection`) fail on macOS because they need a loopback alias and TCP-AO. They fail the same way on unmodified upstream; run them on Linux.

## Test bed

[`lab/`](lab/README.md) holds a containerlab three-router topology (r1 - r2 - r3, eBGP with ECMP), a synthetic full-feed injector, a live metrics dashboard, A/B comparison scripts, and the profiling results in [`lab/results/`](lab/results/PROFILING-sync-pool.md).

## License

Apache License 2.0, inherited from GoBGP. See [LICENSE](LICENSE). The initial MIT license in this repository was replaced because it contains Apache 2.0 licensed GoBGP code.
