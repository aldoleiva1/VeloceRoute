# VeloceRoute blueprint (corrected for the v4 fork)

Replaces the original `aws_vpc_gobgp_wireguard_blueprint.md`. The code is in [`blueprint/main.go`](blueprint/main.go), a separate module that compiles on Linux and macOS against this repository (`cd blueprint && go build .`). Reference only: no error recovery, policy or kernel sync.

## Corrections to the original

| Original | Correct for this fork |
|---|---|
| imports `github.com/osrg/gobgp/v3/...` | `github.com/osrg/gobgp/v4/api` and `.../v4/pkg/server` |
| `AddNeighbor(ctx, &AddNeighborRequest{Neighbor: ...})` | `AddPeer(ctx, &api.AddPeerRequest{Peer: &api.Peer{Conf, Transport}})` |
| `conn.NewDefaultBind()` with no import | add `golang.zx2c4.com/wireguard/conn` |
| `IpcSet` left as a note | shown with env-supplied keys; no keys in code |
| "GoBGP syncs routes to the kernel via netlink" | It does not. Only a Zebra client exists (`pkg/zebra`); netlink is used for socket options only. Kernel FIB sync is new work (ROADMAP E4) |
| `tun.CreateTUN("wg0")` just works | Linux with root or `CAP_NET_ADMIN`; on macOS the name must be `utun` |

New dependencies pulled by the sample, all permissive: wireguard-go (MIT), gvisor (Apache 2.0), google/btree (Apache 2.0), golang.org/x/* (BSD). No GPL or LGPL.

## AWS VPC Route Server: verified facts

Sources: AWS VPC User Guide pages `route-server-how-it-works`, `route-server-terms`, `route-server-tutorial-*` (create, create-peer, create-endpoints, initiate-bgp, iam) and the quotas table at `amazon-vpc-limits#vpc-limits-route-servers`, read in full on 2026-11-11.

| Topic | What AWS documents |
|---|---|
| Who starts the session | **The device initiates BGP. The route server endpoint does not.** Our peer must not be passive |
| Device requirements | an ENI in the VPC, BGP support, ability to initiate sessions |
| ASNs | "Amazon side ASN" for the route server (1-4294967295, private ranges recommended) and a separate "Peer ASN" per peer. All examples use different ASNs (65000 vs 65001), so plan for **eBGP**. AWS does not say it supports iBGP |
| Liveness | per peer: BGP keepalive (default) or BFD. GoBGP has native single-hop BFD (`docs/sources/bfd.md`) |
| Path preference | MED example: lower MED is preferred, and failover moves the route table entry to the standby ENI. Shortest AS path is mentioned in the AWS blog |
| Route tables | route server writes FIB routes into VPC route tables not associated with subnets, subnet route tables and IGW route tables (propagation must be enabled per table). Not virtual private gateway route tables |
| Topology | endpoints are AWS-managed ENIs inside your subnet; max 2 per route server per subnet; AWS recommends one route server peer per endpoint. Peer address must be reachable from the endpoint |
| Persist routes | optional, 1-5 minutes after all sessions drop, so routes survive a brief outage |
| IAM | `ec2:CreateNetworkInterface`, `CreateSecurityGroup`, `Authorize/RevokeSecurityGroupIngress` etc. (see `route-server-iam`); SNS topic permissions if notifications are on |
| Cost | route server endpoints are $0.75 per hour each |
| **Quotas** | **100 routes per peer and 100 routes per route server (FIB).** 5 route servers per VPC; 10 endpoints per route server; 20 peering sessions per network interface |

**Design consequence:** a full internet table can never be advertised to the Route Server. VeloceRoute must export only a small, filtered set (aggregates, anycast and service prefixes) with a hard cap under 100, and alert on exceeding it.

## Still not stated by AWS (needs a test account, tracked in ROADMAP E5-1)

- Whether the route server advertises VPC or other routes back to peers.
- BGP timers, hold time and keepalive values.
- BGP multihop.
- Whether a Route Server peer instance needs source/destination check disabled. Pages only say the route server installs a route to the active instance's ENI. Disabling the check is standard for any instance that forwards traffic it is not the endpoint of (Pattern B), but this is not confirmed for Pattern A.
- Behavior when BFD and BGP timers disagree.
