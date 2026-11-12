#!/bin/bash
# CPU and heap profiles of veloced -fib-sync on r2 during a 900k import (task E4-5).
# Usage: [FEEDGEN_ARGS="-share 40"] ./fib-profile.sh <label> <linux-veloced> [N] [gobgpd-for-lab]
# Output: results/prof/<label>_{cpu,heap}.pb.gz  (go tool pprof -top <file>)
set -u
LABEL=${1:?label}; VELOCED=${2:?binary}; N=${3:-900000}; GOBGPD=${4:-gobgpd-decode}
R1=clab-gobgp3-r1; R2=clab-gobgp3-r2
mkdir -p results/prof
SKIP_BASELINE=1 ./ab.sh fibprof $GOBGPD >/dev/null 2>&1
docker exec $R2 sh -c 'pkill gobgpd; for i in $(seq 1 30); do alive=0; for p in $(pgrep -x gobgpd); do s=$(awk "{print \$3}" /proc/$p/stat 2>/dev/null); [ -n "$s" ] && [ "$s" != Z ] && alive=1; done; [ $alive = 0 ] && break; sleep 1; done'
docker cp "$VELOCED" $R2:/usr/local/bin/veloced >/dev/null
docker exec -d $R2 sh -c "veloced -asn 65002 -router-id 2.2.2.2 -fib-sync -pprof-address 127.0.0.1:6060 -grpc-address 127.0.0.1:50051 > /var/log/veloced.log 2>&1"; sleep 2
docker exec $R2 sh -c 'gobgp neighbor add 10.0.12.1 as 65001' >/dev/null
for i in $(seq 1 30); do [ "$(docker exec $R2 gobgp neighbor | grep -c Establ)" = 1 ] && break; sleep 1; done
sleep 2
docker cp feedgen-linux $R1:/usr/local/bin/feedgen >/dev/null
# 12 s of CPU profile covers the whole import (about 7 s) plus a little idle time
docker exec -d $R2 sh -c 'wget -qO /tmp/cpu.pb.gz "http://127.0.0.1:6060/debug/pprof/profile?seconds=12"'
sleep 1
docker exec $R1 feedgen -addr 127.0.0.1:50051 -n "$N" ${FEEDGEN_ARGS:-} | tail -1
sleep 14
docker exec $R2 sh -c 'wget -qO /tmp/heap.pb.gz "http://127.0.0.1:6060/debug/pprof/heap"; wget -qO /tmp/allocs.pb.gz "http://127.0.0.1:6060/debug/pprof/allocs"'
docker cp $R2:/tmp/cpu.pb.gz results/prof/${LABEL}_cpu.pb.gz; docker cp $R2:/tmp/heap.pb.gz results/prof/${LABEL}_heap.pb.gz; docker cp $R2:/tmp/allocs.pb.gz results/prof/${LABEL}_allocs.pb.gz
docker exec $R2 sh -c 'kill -TERM $(pgrep -x veloced | head -1); sleep 5; rm -f /usr/local/bin/veloced'
SKIP_BASELINE=1 ./ab.sh restore $GOBGPD >/dev/null 2>&1
ls -la results/prof/${LABEL}_*.pb.gz
