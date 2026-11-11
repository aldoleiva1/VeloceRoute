# VeloceRoute roadmap and task list

Source: `aws_vpc_gobgp_wireguard_blueprint.md` (design idea), `CLAUDE.md` (guidelines and improvement tracker), and the lab profiling results in `lab/results/`.
Goal: one static Go binary that embeds the GoBGP control plane and a WireGuard engine, syncs the BGP best paths into the Linux kernel, and peers with the Amazon VPC Route Server.

Legend: **P0** do first, **P1** core feature, **P2** later. `deps` lists task IDs. Every task has a checkable "done when".

## Progress (updated 2026-11-11)

Status of the product features and the task areas. A task is **done** only when its "done when" check is met.

| Area | Status | What exists | Next task |
|---|---|---|---|
| Lab (3 routers, feed injector, dashboard, A/B tooling) | Working | `lab/`; `ab.sh`, `run-ab2.sh`, `summarize.py` | E6-3, E6-1 |
| Blueprint corrected to v4 and checked against AWS docs | Done (A-3) | `docs/veloceroute/blueprint.md`, compiling sample | E5-1 |
| A-1 variance runs | Done | 3x3 upstream vs branch in `lab/results/` | none |
| E1 performance | In progress | Server fixes; encode path (allocations 26 to 10 per UPDATE); decode slab for IPv4 prefixes (40-prefix UPDATE 69 to 24 allocs); table layer (mallocs/route 66.4 to 50.4, CPU -26% on the realistic feed). See `docs/veloceroute/performance.md` | rest of E1-3 (`getOrCreateDest` key, `UpdatePathAttrs`, packer); then E1-6 and E1-7 |
| E1-1 encode `sync.Pool` | Mostly done | Append writer, exact buffers, pooled AS-path scratch. `NewBGPUpdateMessage` and the attribute-presence map are untouched | finish remaining 10 allocations |
| Single static binary (`veloced`, E2) | Not started | none | E2-1 (needs A-4 decision) |
| gRPC API and OpenConfig | Upstream only | GoBGP's own API; no WireGuard or FIB additions | E7 after E3/E4 |
| Linux Netlink kernel table sync (E4) | Not started | none (only a Zebra client exists) | E4-1 |
| WireGuard engine (E3) | Not started | none | E3-1 |
| Amazon VPC Route Server peering (E5) | Not started | facts verified from docs only; no AWS test yet | E5-1 |
| CI, lint, Linux test environment (A-2, A-5, A-6) | Not started | none | A-2 |
| Hardening and release (E8, E9) | Not started | none | after features |

Decisions waiting on the owner are listed at the bottom of this file.

## 0. Blueprint review: what has to change before it is used

| # | Blueprint says | Reality in this fork | Task |
|---|---|---|---|
| 1 | imports `github.com/osrg/gobgp/v3/...` | This fork is module `github.com/osrg/gobgp/v4` | A-3 |
| 2 | `bgpServer.AddNeighbor(ctx, &apiv1.AddNeighborRequest{...})` | The v4 method is `AddPeer(ctx, *api.AddPeerRequest)` | A-3 |
| 3 | sample uses `conn.NewDefaultBind()` | `golang.zx2c4.com/wireguard/conn` is not imported, so it will not compile | A-3 |
| 4 | "Routing Table Sync via netlink" | Nothing in this fork syncs best paths into the kernel. `vishvananda/netlink` is only used for socket options. Only a Zebra client exists (`pkg/zebra`). This is new work | E4 |
| 5 | wireguard-go is available | Not in `go.mod`. New dependency (MIT, compatible with Apache 2.0) | E3 |
| 6 | WireGuard config via `IpcSet` is a "note" | Keys, peers, allowed-IPs, endpoints and reload are the whole feature | E3 |
| 7 | `tun.CreateTUN("wg0")` | Needs root or `CAP_NET_ADMIN` on Linux. macOS names the device `utunN`. Cannot run in the current macOS-only test setup, so the lab must run Linux | E6 |
| 8 | Route Server session is an "interior BGP session" | **Corrected.** AWS configures a separate Amazon-side ASN and Peer ASN, and every example uses different ASNs, so plan for eBGP. AWS does not state iBGP support. The device must also initiate the session; endpoints never do | E5-2 |
| 9 | "Disable source/dest check on the ENI" (Pattern B) | Standard for a forwarder (Pattern B). Not stated by AWS for Route Server peers (Pattern A); still **unverified**, test in E5-1 | E5-1, E5-3 |
| 10 | Implied: advertise learned routes to the Route Server | **New constraint:** AWS quotas are **100 routes per peer and 100 per route server**. A full feed can never be advertised; export must be filtered and capped | E5-9 |

AWS facts (full list with sources in `docs/veloceroute/blueprint.md`) confirmed from AWS docs/blog: BFD between the application and the Route Server endpoints is optional; two Route Server endpoints per subnet, in the same subnet as the peers; best path chosen by shortest AS path (MED mentioned); the route server updates VPC route tables (not associated with subnets), subnet route tables and IGW route tables; virtual private gateway route tables are not supported. Not stated on the pages read: BGP timers and hold time, whether VPC routes are advertised back to peers, multihop support, route and prefix quotas (see AWS "Route server quotas").

## Phase 0: foundations (P0)

| ID | Task | Done when | deps |
|---|---|---|---|
| A-1 | **Done.** Variance runs: repeat the 900k baseline 3x on upstream and 3x on our branch | `lab/results/` has 6 runs and a table with mean and spread for allocs/route, GC cycles, CPU, converge time | none |
| A-2 | Linux test environment so `go test -race ./pkg/server/...` is fully green (4 tests fail on macOS: loopback alias 127.0.0.201 and TCP-AO) | Tests run in a Linux container with 0 failures; documented command in `lab/README.md` | none |
| A-3 | ~~Correct the blueprint to v4 API~~ **Done:** `docs/veloceroute/blueprint.md` and `docs/veloceroute/blueprint/main.go` (compiles on Linux and macOS) | Done | none |
| A-4 | Decide the Go module path (keep `osrg/gobgp/v4` internally, or rename to `github.com/aldoleiva1/VeloceRoute`) | Decision recorded; if renamed, one mechanical commit touching ~128 files, build and tests green | none |
| A-5 | Install `golangci-lint`; add `make lint test bench` targets matching CLAUDE.md | `make lint` runs clean on changed packages | none |
| A-6 | Add GitHub Actions: build, `go test ./pkg/packet/bgp/...`, `go test -race ./pkg/server/...` on Linux, lint | Green check on `main` | A-2, A-5 |
| A-7 | License hygiene: third-party notice for wireguard-go (MIT) and any new deps; check no GPL/LGPL (CLAUDE.md rule 1) | `NOTICE`/`THIRD_PARTY.md` and a CI license check | none |

## E1. Performance work (from CLAUDE.md tracker and profiling)

Baseline to beat (r2, 900k import, mean of 3, upstream): 193.8 mallocs/route, 8.8 KB/route, 57 GC cycles, 70 s CPU, 23.5 s converge. After the two server fixes (mean of 3): 162.2 mallocs, 5.5 KB, 33.7 GC cycles, 52.8 s, 17.8 s. After the encode work (append build vs scratch build, n=4 vs 3): 154.2 to 139.9 mallocs/route. On a realistic feed (`feedgen -share 40`, about 37 prefixes per UPDATE; mean of 3, upstream vs all changes): 97.8 to 66.4 mallocs/route, 19.9 to 14.2 s CPU, 9.4 to 7.3 s converge. After the table-layer work (previous build vs new, n=3): 66.4 to 50.4 mallocs/route, 15.5 to 11.4 s CPU, 8.0 to 6.6 s converge. Details: `docs/veloceroute/performance.md`.

| ID | Task | Done when | deps |
|---|---|---|---|
| E1-1 | **Mostly done.** `sync.Pool` for UPDATE **encode** buffers and temp slices: `BGPUpdate.Serialize`, `PathAttribute.Serialize`, `PathAttributeAsPath.Serialize`, `NewBGPUpdateMessage` | `go test -bench` in `pkg/packet/bgp` shows fewer allocs/op; lab run shows lower mallocs/route; tests pass with `-race` | A-1 |
| E1-2 | **Partly done** (IPv4 prefix slab; IPv6 and per-attribute allocations remain). `sync.Pool` for UPDATE **decode** scratch (`BGPUpdate.DecodeFromBytes`, `parseBody`) with safe release rules (nothing may alias a pooled buffer after release) | Same as E1-1, plus a test that fails if a pooled buffer is retained | E1-1 |
| E1-3 | **Partly done** (bitmap, `PrependAsn`/`cloneAsPath`, `dstsToPaths`, `GetPathAttrs` speed; `Path.Clone` and `getOrCreateDest` key remain). Table layer allocations: lazy `NewBitmap`, avoid `Path.Clone` for read-only use, `clonePathList`, `dstsToPaths` | Profile diff shows these frames drop; tests pass | A-1 |
| E1-4 | **Done.** `notifyBestWatcher`: skip building events when there are no watchers | No allocations from it with zero watchers; tests pass | none |
| E1-5 | **Done.** `GOGC` / `GOMEMLIMIT` experiments on the 900k run (no code change) | Table of settings vs CPU, RSS, converge time; recommended default documented | A-1 |
| E1-6 | Zero-copy path for `WatchEvent()` for local consumers (tracker item). **`proto/api/gobgp.proto` first**, regenerate stubs, then code | Local in-process watcher receives events without protobuf marshalling; benchmark shows lower cost; gRPC watchers unchanged. Reused by E4 (kernel sync) | none |
| E1-7 | Peer FSM scaling: decouple the central loop so sessions advance in parallel (tracker item). Design doc first | Design doc reviewed; then implementation behind a flag; `-race` clean; lab test with many peers shows scaling | E1-1..E1-4, A-2 |
| E1-8 | Many-peer scale test: N peers (e.g. 100 and 1000) using lightweight injectors | Result table in `lab/results/` | E6-1 |

## E2. Embedded runtime: `veloced` single binary (P1)

| ID | Task | Done when | deps |
|---|---|---|---|
| E2-1 | `cmd/veloced` skeleton: `server.NewBgpServer`, `StartBgp`, signal handling, structured logging | Binary starts and serves the existing gRPC API | A-3 |
| E2-2 | Config schema (TOML/YAML) for global BGP, neighbors, WireGuard interfaces, kernel sync, AWS. New fields go into `proto/api/gobgp.proto` first (CLAUDE.md rule 4), then regenerate | Config file loads and validates; invalid files give clear errors | A-4 |
| E2-3 | Lifecycle: ordered start (WG up, BGP up, FIB sync) and ordered stop (withdraw routes, remove kernel routes, close tunnels); hot reload on SIGHUP | Integration test starts, reloads and stops cleanly with no leftover interfaces or routes | E3-1, E4-1 |
| E2-5 | Set the Go GC defaults at startup (`debug.SetGCPercent(200)` and `debug.SetMemoryLimit` from the cgroup or instance memory, about 75-80%), overridable by `GOGC`/`GOMEMLIMIT` | Starts with the E1-5 recommended settings; env vars still win; a test checks the limit is derived from the container memory | E2-1, E1-5 |
| E2-4 | Static build (`CGO_ENABLED=0`), size and startup check | `veloced` runs on a minimal image with no libc | E2-1 |

## E3. WireGuard engine (P1)

| ID | Task | Done when | deps |
|---|---|---|---|
| E3-1 | Tunnel provisioning with `wireguard-go` (`tun`, `device`, `conn`): create `wgN`, MTU 1420, address assignment | Interface appears, comes up, and passes traffic between two containers | E2-1 |
| E3-2 | Key management: generate keypair, store private key with 0600 permissions, never in the main config or logs, support loading from a file or secret store | Keys survive restart; a test asserts no key material in logs | E3-1 |
| E3-3 | Peer configuration: endpoints, allowed-IPs, keepalive, preshared key, via `IpcSet` or `wgctrl` | Peers configured from the config file; handshake completes | E3-1 |
| E3-4 | BGP peering across the tunnel: derive neighbor and local addresses from the WG link, restrict the session to the WG interface | BGP session Established over `wg0` in the lab | E3-3, E2-2 |
| E3-5 | Dynamic peers: add and remove WG peers and BGP neighbors at runtime through the API | Add/remove works without restart; no leaked peers | E3-4 |
| E3-6 | Kernel-WireGuard option on Linux (via `wgctrl`) as an alternative to userspace | Selectable in config; same tests pass in both modes | E3-3 |
| E3-7 | Allowed-IPs policy: decide how BGP-learned routes interact with WG allowed-IPs (WG drops packets not in allowed-IPs) | Documented rule plus a test that a learned prefix is actually forwarded across the tunnel | E3-4, E4-2 |
| E3-8 | Tunnel health: handshake age, rx/tx bytes, endpoint changes exported as metrics; optional BFD or tight BGP timers for fast failure detection | Metrics visible in the dashboard; failover test (E6-4) passes | E3-3 |

## E4. Linux kernel FIB sync via Netlink (P1)

| ID | Task | Done when | deps |
|---|---|---|---|
| E4-1 | Netlink route programmer: subscribe to best-path changes (`WatchEvent`/`WatchBestPath`, ideally the E1-6 zero-copy path), add/replace/delete routes with `vishvananda/netlink` | Learned prefixes appear in `ip route` and are removed on withdraw | E2-1 |
| E4-2 | Nexthop resolution and interface binding (WG interface vs ENI); skip or defer unresolvable nexthops | Test with reachable and unreachable nexthops | E4-1 |
| E4-3 | ECMP: program multipath routes when GoBGP has multiple best paths (lab anycast case) | `ip route` shows 2 nexthops for 192.0.2.1/32 | E4-1 |
| E4-4 | Own the routes: tag with a dedicated routing protocol ID, optional dedicated table; reconcile on start (remove stale), clean up on exit | Crash and restart test leaves no stale routes | E4-1 |
| E4-5 | Bulk programming performance: batching, avoid one syscall per route, measure with the 900k feed | Time and CPU to program N routes documented; no blocking of the BGP loop (single-writer rule, CLAUDE.md rule 3) | E4-1, E1-6 |
| E4-6 | Tests in a Linux network namespace (CI-capable) | Netns test suite runs in CI | A-6, E4-1 |

## E5. AWS integration (P1; needs a real AWS account, approval and cost awareness before any create)

| ID | Task | Done when | deps |
|---|---|---|---|
| E5-1 | Verify the remaining Route Server unknowns in a test account (docs are now read: eBGP examples, device initiates, 100-route quotas, BFD optional). Open: does it advertise routes back, timers and hold time, multihop, source/dest check for Pattern A | Findings written into `docs/veloceroute/blueprint.md` | none |
| E5-2 | Pattern A: peer `veloced` with Route Server endpoints; advertise selected prefixes; confirm route tables update | Route table in the VPC shows the advertised prefix pointing at the instance ENI | E2-2, E5-1 |
| E5-3 | Pattern B: transit router: disable source/dest check, kernel forwarding on, FIB sync | Instance forwards traffic between two subnets using BGP-learned routes | E4-1 |
| E5-4 | Optional BFD to Route Server endpoints (AWS says BFD is optional) | Failover time measured with and without BFD | E5-2 |
| E5-5 | Infrastructure as code (Terraform): VPC, subnets, Route Server, endpoints, peers, two instances, security groups (UDP for WG, TCP 179), IAM | `terraform apply` and `destroy` work from a clean account | E5-1 |
| E5-6 | Instance metadata discovery: private IP, ENI, subnet, AZ to fill router ID and local address automatically | `veloced` starts with minimal config on EC2 | E2-2 |
| E5-7 | Multi-AZ active/standby behavior: two instances, one preferred by AS-path or MED | Kill the active instance; route table switches; time recorded | E5-2 |
| E5-9 | Export policy for the Route Server: allow-list of prefixes, hard cap below 100 routes per peer, alert and refuse to exceed (AWS quota: 100 per peer and per route server). Use GoBGP policy (`docs/sources/policy.md`) | Test: advertising 101 prefixes is blocked and logged; lab test with a full feed shows only the allowed set reaches the Route Server peer | E2-2 |
| E5-10 | Session setup details: device-initiated sessions (no passive mode), one peer per endpoint (two endpoints per subnet), persist-routes interaction with graceful restart | Config template and a test showing sessions come up from our side only | E5-2 |
| E5-8 | Cost and cleanup guard for tests (tags, TTL, destroy script) | Documented and used for every AWS test | E5-5 |

## E6. Lab and test bed (extends what exists)

| ID | Task | Done when | deps |
|---|---|---|---|
| E6-1 | Add an FRR peer for interoperability and a many-peer injector | FRR session Established; routes exchanged in both directions | none |
| E6-2 | Put WireGuard links between lab routers (Linux only, in the containers) | r1-r2-r3 peer over `wg` interfaces in the lab | E3-4 |
| E6-3 | Re-add anycast prefix automatically in startup config (it is lost on redeploy) | ECMP visible on r2 after every deploy | none |
| E6-4 | Failover and convergence test: drop r1 or r3, time to withdraw and reconverge, record in `lab/results/` | Scripted test and numbers | E6-3 |
| E6-5 | Dashboard additions: WG handshake age, kernel routes, convergence timer, per-run history | Visible in the dashboard | E3-8, E4-1 |
| E6-6 | Make `baseline.sh` accept a binary path and run A/B comparisons automatically | One command prints a before/after table | A-1 |

## E7. API and observability

| ID | Task | Done when | deps |
|---|---|---|---|
| E7-1 | gRPC additions for WireGuard (list peers, add/remove peer, show keys' public half only). **Proto first**, then regenerate | `gobgp`-style CLI or `grpcurl` can manage WG peers | E3-5 |
| E7-2 | Prometheus metrics for WG and FIB sync in the same `:6060/metrics` endpoint | Metrics documented and scraped by the dashboard | E3-8, E4-1 |
| E7-3 | OpenConfig mapping for the new settings where a model exists | Documented mapping and config round trip test | E2-2 |

## E8. Security and hardening

| ID | Task | Done when | deps |
|---|---|---|---|
| E8-1 | gRPC bound to localhost by default; TLS and auth options. (The lab currently binds `0.0.0.0:50051` on the private lab network only) | Default config is not reachable off-host | E2-1 |
| E8-2 | Drop privileges after creating the TUN and netlink handles; keep only `CAP_NET_ADMIN` (and bind capability if needed) | Process runs unprivileged except those caps | E3-1, E4-1 |
| E8-3 | Fuzz tests for UPDATE decode/encode in `pkg/packet/bgp` (especially after E1-2 pooling) | Fuzz targets run in CI for a short budget | E1-2 |
| E8-4 | `govulncheck` and dependency review in CI | Clean report or tracked exceptions | A-6 |
| E8-5 | Secrets handling review: private keys, preshared keys, AWS credentials (prefer instance roles) | Written review; no secrets in repo or logs | E3-2 |

## E9. Packaging and release

| ID | Task | Done when | deps |
|---|---|---|---|
| E9-1 | Versioning scheme for VeloceRoute (decide the starting version) and where `version.go` stands relative to upstream | Decision recorded; applied in one commit | A-4 |
| E9-2 | Release builds (linux amd64/arm64 static), checksums, GitHub release workflow | Tagged release produces binaries | A-6, E2-4 |
| E9-3 | Container image and systemd unit with the right capabilities | Image runs `veloced` in the lab and on EC2 | E2-4, E8-2 |
| E9-4 | Docs: quick start (lab), AWS deploy guide, config reference, upstream-sync procedure (`git fetch upstream`) | Docs reviewed by following them from scratch | all |

## Suggested order

1. **Now:** A-1, A-2, A-5, A-3 (measure properly and fix tooling).
2. **Performance track:** E1-1, E1-4, E1-2, E1-3, E1-5, then E1-6; E1-7 last (riskiest).
3. **Feature track (can start in parallel):** E2-1 -> E3-1/E3-2/E3-3 -> E3-4 -> E4-1/E4-2/E4-3 -> E6-2 -> E3-8, E4-4.
4. **AWS track (after E3 and E4 work in the lab):** E5-1 first (cheap, mostly reading), then E5-5, E5-2, E5-3, E5-4, E5-7.
5. **Hardening and release:** E8, E9, E7 in step with each feature.

## Open questions for the owner

1. Keep the upstream module path or rename to the VeloceRoute path (A-4)? Renaming makes future `git merge upstream/master` noisier.
2. Userspace `wireguard-go` only, or also kernel WireGuard on Linux (E3-6)? Kernel mode is faster on EC2.
3. IPv6 and EVPN: keep as-is from upstream, or out of scope for the first release?
4. Which AWS account and region will be used for E5, and what is the cost ceiling?
5. Target instance types and architecture (arm64 Graviton vs x86) for the first release.
