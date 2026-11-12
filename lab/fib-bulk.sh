#!/bin/bash
# Bulk kernel programming at scale (task E4-5): inject N routes on r1, run veloced -fib-sync on r2, and time
# (a) BGP convergence on r2 and (b) the kernel table reaching N routes, plus veloced's CPU time and peak memory.
# NO_FIB=1 runs veloced WITHOUT -fib-sync as a control (BGP and watcher cost only).
# Usage: [NO_FIB=1] [FEEDGEN_ARGS="-share 40"] [VELOCED_ARGS="..."] [VELOCED_ENV="GOGC=200"] ./fib-bulk.sh <label> <linux-veloced> [N] [gobgpd-for-lab]
# Restores gobgpd on all routers afterwards. Output: results/<label>-<timestamp>.txt
set -u
LABEL=${1:?label}; VELOCED=${2:?veloced binary}; N=${3:-900000}; GOBGPD=${4:-gobgpd-decode}
R1=clab-gobgp3-r1; R2=clab-gobgp3-r2
[ "${NO_FIB:-}" = 1 ] && FIBFLAG=""
OUT=results/${LABEL}-$(date +%Y%m%d-%H%M%S).txt
# The kernel's own count of routes in the main table (it includes a few kernel routes, so compare with the count before).
kcount() { docker exec $R2 sh -c "awk '/^Main:/{f=1} f&&/Prefixes:/{print \$2; exit}' /proc/net/fib_triestat" 2>/dev/null; }
bcount() { docker exec $R2 gobgp global rib summary -a ipv4 2>/dev/null | awk '/Destination/{gsub(",","",$2); print $2; exit}'; }
cpu() { docker exec $R2 sh -c 'p=$(pgrep -x veloced | head -1); awk "{print (\$14+\$15)/100}" /proc/$p/stat'; }
{
echo "label=$LABEL n=$N date=$(date -u +%FT%TZ) feedgen_args=${FEEDGEN_ARGS:-} veloced_args=${VELOCED_ARGS:-} env=${VELOCED_ENV:-}"
SKIP_BASELINE=1 ./ab.sh fibbulk $GOBGPD >/dev/null 2>&1
docker exec $R2 sh -c 'pkill gobgpd; for i in $(seq 1 30); do alive=0; for p in $(pgrep -x gobgpd); do s=$(awk "{print \$3}" /proc/$p/stat 2>/dev/null); [ -n "$s" ] && [ "$s" != Z ] && alive=1; done; [ $alive = 0 ] && break; sleep 1; done'
docker cp "$VELOCED" $R2:/usr/local/bin/veloced >/dev/null
docker exec -d $R2 sh -c "${VELOCED_ENV:-} veloced -asn 65002 -router-id 2.2.2.2 ${FIBFLAG--fib-sync} -grpc-address 127.0.0.1:50051 ${VELOCED_ARGS:-} > /var/log/veloced.log 2>&1"; sleep 2
docker exec $R2 sh -c 'gobgp neighbor add 10.0.12.1 as 65001' >/dev/null
for i in $(seq 1 30); do [ "$(docker exec $R2 gobgp neighbor | grep -c Establ)" = 1 ] && break; sleep 1; done
sleep 2
docker cp feedgen-linux $R1:/usr/local/bin/feedgen >/dev/null
K0=$(kcount); echo "kernel_prefixes_before=$K0"
C0=$(cpu); S=$(date +%s.%N)
docker exec $R1 feedgen -addr 127.0.0.1:50051 -n "$N" ${FEEDGEN_ARGS:-}
FEED_DONE=$(date +%s.%N); echo "feed_sent_seconds=$(echo "$FEED_DONE - $S" | bc)"
tb=""; tk=""
while true; do
  now=$(date +%s.%N)
  if [ -z "$tb" ]; then b=$(bcount); [ "${b:-0}" -ge "$N" ] && tb=$(echo "$now - $S" | bc); fi
  if [ "${NO_FIB:-}" = 1 ]; then tk=NA; elif [ -z "$tk" ]; then k=$(kcount); [ $((${k:-0} - K0)) -ge "$N" ] && tk=$(echo "$now - $S" | bc); fi
  [ -n "$tb" ] && [ -n "$tk" ] && break
  if [ "$(echo "$now - $S > 300" | bc)" = 1 ]; then echo "TIMEOUT bgp=${b:-?} kernel=${k:-?}"; break; fi
  sleep 0.5
done
C1=$(cpu)
echo "bgp_converged_r2_seconds=${tb:-NA}"
echo "kernel_programmed_seconds=${tk:-NA}"
echo "kernel_lag_after_bgp_seconds=$( [ -n "$tb" ] && [ "$tk" != NA ] && [ -n "$tk" ] && echo "$tk - $tb" | bc || echo NA)"
echo "veloced_cpu_seconds=$(echo "$C1 - $C0" | bc)"
echo "veloced_peak_rss_mb=$(docker exec $R2 sh -c 'p=$(pgrep -x veloced | head -1); awk "/VmHWM/{print int(\$2/1024)}" /proc/$p/status')"
echo "kernel_routes_added=$(( $(kcount) - K0 ))"
echo "-- veloced log tail"; docker exec $R2 sh -c 'grep -v "passive connection" /var/log/veloced.log | tail -5'
echo "-- stop: time for SIGTERM to remove every route"
T0=$(date +%s.%N)
docker exec $R2 sh -c 'kill -TERM $(pgrep -x veloced | head -1); for i in $(seq 1 600); do pgrep -x veloced >/dev/null || exit 0; s=$(awk "{print \$3}" /proc/$(pgrep -x veloced | head -1)/stat 2>/dev/null); [ "$s" = Z ] && exit 0; sleep 0.5; done'
echo "stop_seconds=$(echo "$(date +%s.%N) - $T0" | bc)  kernel_routes_left_after_stop=$(( $(kcount) - K0 ))"
docker exec $R2 sh -c 'rm -f /usr/local/bin/veloced'
} 2>&1 | tee "$OUT"
SKIP_BASELINE=1 ./ab.sh restore $GOBGPD >/dev/null 2>&1
echo "saved $OUT"
