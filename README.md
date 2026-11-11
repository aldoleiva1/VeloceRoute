# VeloceRoute
VeloceRoute is a lightweight software router for AWS VPC environments. It embeds a GoBGP control plane and WireGuard engine into a unified Go runtime for dynamic routing over secure tunnels.

Features:
- Single static binary
- gRPC API & OpenConfig support
- Seamless Amazon VPC Route Server peering
- Linux Netlink kernel table sync

## Status

Early stage. The repository starts from upstream [osrg/gobgp](https://github.com/osrg/gobgp) v4.11.0 (commit `f7daa917`, remote `upstream`) and is being tuned for allocation and GC behavior under full-table imports before the new features are added. The original GoBGP README is kept in [README-GoBGP.md](README-GoBGP.md).

Changes on top of upstream so far:
- `pkg/server/peer.go`: removed a per-UPDATE peer-config copy and made debug-log arguments lazy. About 36% fewer bytes allocated per route and 33% fewer GC cycles on a transit router importing 900k routes.

## Test bed

[`lab/`](lab/README.md) holds a containerlab three-router topology (r1 - r2 - r3, eBGP with ECMP), a synthetic full-feed injector, a live metrics dashboard, and the profiling results in [`lab/results/`](lab/results/PROFILING-sync-pool.md).

## License

Apache License 2.0, inherited from GoBGP. See [LICENSE](LICENSE). The initial MIT license in this repository was replaced because it contains Apache 2.0 licensed GoBGP code.
