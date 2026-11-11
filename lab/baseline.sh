#!/bin/bash
# Full-feed baseline: inject N /24s on r1, wait for r2 and r3 to converge, record runtime metrics.
# Usage: [FEEDGEN_ARGS="-share 40"] ./baseline.sh [N] [label]
# FEEDGEN_ARGS are passed to feedgen (-share K: K prefixes per attribute set).
N=${1:-900000}; LABEL=${2:-baseline}; OUT=results/${LABEL}-$(date +%Y%m%d-%H%M%S).txt
KEYS='^go_memstats_(alloc_bytes|alloc_bytes_total|mallocs_total|heap_inuse_bytes|sys_bytes) |^go_gc_duration_seconds(_sum|_count) |^process_(resident_memory_bytes|cpu_seconds_total) |^go_goroutines '
snap() { for r in r1 r2 r3; do echo "# $1 $r"; docker exec clab-gobgp3-$r sh -c "wget -qO- 127.0.0.1:6060/metrics" | grep -E "$KEYS"; done; }
rcount() { docker exec clab-gobgp3-$1 gobgp global rib summary -a ipv4 2>/dev/null | awk '/Destination/{gsub(",","",$2); print $2; exit}'; }
{
echo "label=$LABEL n=$N date=$(date -u +%FT%TZ)"
snap before
docker cp feedgen-linux clab-gobgp3-r1:/usr/local/bin/feedgen >/dev/null
echo "feedgen_args=${FEEDGEN_ARGS:-}"
S=$(date +%s.%N)
docker exec clab-gobgp3-r1 feedgen -addr 127.0.0.1:50051 -n "$N" $FEEDGEN_ARGS
while true; do c2=$(rcount r2); c3=$(rcount r3); [ "${c2:-0}" -ge "$N" ] && [ "${c3:-0}" -ge "$N" ] && break; sleep 1; done
E=$(date +%s.%N)
echo "converged_r2_r3_seconds=$(echo "$E - $S" | bc)"
echo "updates_rcvd_r2_from_r1=$(docker exec clab-gobgp3-r2 sh -c 'gobgp neighbor 10.0.12.1' | awk '/Updates:/{print $3; exit}')"
snap after
} | tee "$OUT"
echo "saved $OUT"
