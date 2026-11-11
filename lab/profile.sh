#!/bin/bash
# Allocation profile of r2 during one import. Usage: [FEEDGEN_ARGS=...] ./profile.sh <label> <linux-gobgpd> [N]
# Restarts the daemons, snapshots r2's allocs profile, injects N routes from r1, waits for r2 and r3,
# snapshots again. Profiles go to results/prof/<label>_{before,after}.pb.gz; diff with:
#   go tool pprof -sample_index=alloc_objects -base results/prof/<label>_before.pb.gz results/prof/<label>_after.pb.gz
set -e
LABEL=$1; BIN=$2; N=${3:-900000}
SKIP_BASELINE=1 ./ab.sh "$LABEL-prof" "$BIN" "$N" >/dev/null
sleep 3
mkdir -p results/prof
snap() { docker exec clab-gobgp3-r2 sh -c "wget -qO- 127.0.0.1:6060/debug/pprof/allocs" > results/prof/${LABEL}_$1.pb.gz; }
rcount() { docker exec clab-gobgp3-$1 gobgp global rib summary -a ipv4 2>/dev/null | awk '/Destination/{gsub(",","",$2); print $2; exit}'; }
docker cp feedgen-linux clab-gobgp3-r1:/usr/local/bin/feedgen >/dev/null
snap before
docker exec clab-gobgp3-r1 feedgen -addr 127.0.0.1:50051 -n "$N" $FEEDGEN_ARGS
while true; do c2=$(rcount r2); c3=$(rcount r3); [ "${c2:-0}" -ge "$N" ] && [ "${c3:-0}" -ge "$N" ] && break; sleep 1; done
snap after
ls -la results/prof/${LABEL}_*.pb.gz
