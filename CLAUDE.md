<html><body><h1>CLAUDE.md - GoBGP Fork Customization Context</h1>

<h2>Project Intent</h2>
<p>This repository is a customized fork of GoBGP (osrg/gobgp) optimized for performance scaling across container network interfaces, virtualized environments, and anycast top-of-rack deployments.</p>

<h2>Build and Test Automation</h2>
<ul>
  <li>Build binaries: <code>go build -v -o bin/gobgpd ./cmd/gobgpd &amp;&amp; go build -v -o bin/gobgp ./cmd/gobgp</code></li>
  <li>Run core tests: <code>go test -v -race ./pkg/server/...</code></li>
  <li>Run packet tests: <code>go test -v ./pkg/packet/bgp/...</code></li>
  <li>Lint code: <code>golangci-lint run ./...</code></li>
</ul>

<h2>Codebase Architecture Reference</h2>
<ul>
  <li><code>pkg/server/</code>: Core daemon event loops, Peer FSM mechanics, and RIB coordination.</li>
  <li><code>pkg/packet/bgp/</code>: Wire serialization/deserialization for BGP packets, EVPN path attributes, and communities.</li>
  <li><code>api/</code>: Protobuf schemas (<code>gobgp.proto</code>) and generated gRPC service frameworks.</li>
  <li><code>pkg/zebra/</code>: Kernel interface bridging forwarding tables with host network stacks.</li>
</ul>

<h2>System-Level Engineering Guidelines</h2>
<ol>
  <li><b>Permissive Compliance Only:</b> Do not import GPL/LGPL code or dependencies into internal or core packages. All code additions must match Apache 2.0.</li>
  <li><b>Allocation Management:</b> Minimize internal memory allocations (<code>go test -bench=.</code>) inside critical paths in <code>pkg/packet/bgp/</code> to preserve CPU cycles during massive table updates.</li>
  <li><b>Concurrency Protocols:</b> Rely on single-writer message channels or explicit <code>sync.RWMutex</code> constructs when writing updates to the localized routing information base (RIB).</li>
  <li><b>Protobuf Pipeline:</b> Modify <code>api/gobgp.proto</code> first when adding configuration parameters or operational endpoints, and re-generate stubs before updating code files.</li>
</ol>

<h2>Known &amp; Most Requested Improvements Tracker</h2>
<ul>
  <li><b>High Allocation &amp; Memory Footprint Reductions:</b> Integrate <code>sync.Pool</code> structures inside the path attribute serialization layer to reduce heavy Go garbage collection cycles during full-feed internet table imports.</li>
  <li><b>Protobuf Serialization Bottlenecks:</b> Optimize streaming watch APIs (<code>WatchEvent()</code>) to bypass costly external marshalling overhead for zero-copy local container overlay orchestrators.</li>
  <li><b>Peer FSM Scaling Bottlenecks:</b> Decouple the centralized loop handling individual peer state machines so sessions can advance asynchronously across multi-core processors.</li>
</ul></body></html>