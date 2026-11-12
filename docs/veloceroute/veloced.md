# veloced: the VeloceRoute daemon

`veloced` is the single binary VeloceRoute is building toward. Today it is a
skeleton (roadmap task E2-1) that embeds the GoBGP engine and its gRPC API, with
structured logging, signal handling and an ordered start and stop, plus an optional
Linux kernel route sync stage ([fib.md](fib.md), task E4-1). WireGuard and Amazon VPC
Route Server peering are not in it yet; each will be added as a lifecycle stage
(tasks E3, E5).

Source: [`cmd/veloced/`](../../cmd/veloced/).

## Build and run

```bash
CGO_ENABLED=0 go build -o bin/veloced ./cmd/veloced            # static; add GOOS=linux GOARCH=arm64 for the lab
./bin/veloced -asn 65001 -router-id 10.0.0.1 -listen-port -1   # -1 = do not listen for BGP sessions
./bin/gobgp -p 50051 global                                    # the existing CLI talks to it
```

BGP's own port is 179, which needs root or `CAP_NET_BIND_SERVICE`; use
`-listen-port -1` to run without it (outgoing sessions only).

## Flags

| flag | default | meaning |
|---|---|---|
| `-asn` | required | local AS number (32-bit) |
| `-router-id` | required | BGP router ID, an IPv4 address |
| `-listen-port` | 179 | TCP port for incoming BGP sessions; -1 disables the listener |
| `-grpc-address` | `127.0.0.1:50051` | gRPC API address, `host:port` or `unix://path`, comma separated for several. Localhost by default (task E8-1); `gobgpd` defaults to all interfaces |
| `-log-level` | info | debug, info, warn, error |
| `-log-format` | text | text or json |
| `-version` | | print the version and exit |
| `-fib-sync`, `-fib-protocol`, `-fib-table`, `-fib-metric` | off, 201, 254, 20 | kernel route sync (Linux, needs `CAP_NET_ADMIN`); see [fib.md](fib.md) |
| `-fib-retry-interval` | 30s | retry timer for routes whose next hop was unreachable; network changes also trigger a retry and a check that the kernel still holds our routes; see [fib.md](fib.md) |
| `-multipath`, `-fib-max-paths` | off, 64 | keep every equal-cost path; with `-fib-sync` build ECMP kernel routes (at most N next hops); see [fib.md](fib.md) |

Invalid flags exit with status 2 and a message listing every problem. A real
config file (global BGP, neighbors, WireGuard, kernel sync, AWS) is task E2-2.

## What it does

1. Validates the flags, builds the logger.
2. Starts the stages in order. `bgp`: it checks the gRPC address can be bound,
   creates the GoBGP server (`server.NewBgpServer`), runs `Serve()` and calls
   `StartBgp` with the ASN, router ID and listen port. `fib` (only with `-fib-sync`):
   it checks the privilege, removes stale routes and starts watching best paths.
3. Waits for SIGINT or SIGTERM. SIGHUP is logged as "not implemented yet"
   (reload is task E2-3).
4. Stops the stages in **reverse** order, each with a 30 second budget overall.
   The `bgp` stage calls `BgpServer.Stop()`, which withdraws routes and closes
   sessions (`StopBgp`), then stops BFD and the gRPC server. Every stage is stopped
   even if an earlier one failed, and all errors are returned together.

If a stage fails to start, the stages that already started are stopped in reverse
order and the start error is returned, so nothing is left running.

## Adding a stage

A stage is a name plus `start` and `stop` functions (`cmd/veloced/lifecycle.go`).
Add it to the list in `run()` in the position its dependencies need, for example
WireGuard before BGP (so tunnel addresses exist) and kernel route sync after BGP
(so it can subscribe to best paths). The lifecycle tests show the contract.

## Verification

`cmd/veloced/veloced_test.go` (run with `go test -race ./cmd/veloced`):

- the lifecycle starts in order, stops in reverse, unwinds on a failed start, and
  keeps stopping after a failed stop;
- flag validation (missing or invalid ASN, router ID, port, level, format);
- the roadmap's "done when" for E2-1: the real daemon starts, answers `GetBgp`
  over gRPC with the configured ASN and router ID, and on cancel (what SIGTERM does)
  shuts down with no warning or error in the log and the API port closed;
- the daemon refuses to start if the gRPC port is already taken.

Also checked by hand: the macOS binary and a static linux/arm64 binary (25 MB,
`CGO_ENABLED=0`) both start, answer the existing `gobgp` CLI, log a clean ordered
shutdown on SIGTERM, and the Linux one runs inside the minimal Alpine lab image
(`ldd` reports it is not a dynamic program, so it needs no libc).

## Known limits

- **gRPC bind check is a pre-check.** GoBGP itself only logs a failed gRPC bind and
  carries on without an API. `veloced` tries to bind each TCP address first and
  refuses to start if one is taken. Another process could still take the port
  between that probe and GoBGP's own bind; then GoBGP logs the error and the daemon
  runs without an API. Unix sockets are not probed.
- No TLS or authentication on the gRPC API yet (task E8-1); keep it on localhost.
- No config file, reload, WireGuard or AWS support yet. Peers can only be added through the gRPC API (for example with the `gobgp` CLI).
- The Go module path is still `github.com/osrg/gobgp/v4` (decision A-4 is open).
