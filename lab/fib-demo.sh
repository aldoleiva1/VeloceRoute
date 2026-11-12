#!/bin/bash
# Live test of kernel route sync (task E4-1) in the 3-router lab.
# Usage: ./fib-demo.sh <linux-veloced-binary> [gobgpd-binary-for-restore]
# Swaps veloced -fib-sync in for r2's gobgpd, peers it with r1 and r3, and checks r2's kernel table:
# learned prefixes appear (proto 201), an overlapping connected network is not disturbed, an unreachable
# next hop is skipped, withdrawals remove routes, a clean stop removes everything, and routes left by a
# crash (kill -9) are removed on the next start. Restores the lab afterwards.
set -u
VELOCED=${1:?usage: ./fib-demo.sh <linux-veloced-binary> [gobgpd]}; GOBGPD=${2:-gobgpd-decode}
R1=clab-gobgp3-r1; R2=clab-gobgp3-r2; R3=clab-gobgp3-r3
pass=0; fail=0
check() { # check <description> <command run on r2> <expected output>
  got=$(docker exec $R2 sh -c "$2" 2>&1); if [ "$got" = "$3" ]; then echo "PASS  $1"; pass=$((pass+1)); else echo "FAIL  $1"; echo "      want: $3"; echo "      got:  $got"; fail=$((fail+1)); fi; }
start_veloced() { docker exec -d $R2 sh -c 'veloced -asn 65002 -router-id 2.2.2.2 -fib-sync -grpc-address 127.0.0.1:50051 > /var/log/veloced.log 2>&1'; sleep 2; }
add_peers() { docker exec $R2 sh -c 'gobgp neighbor add 10.0.12.1 as 65001; gobgp neighbor add 10.0.23.2 as 65003' >/dev/null; for i in $(seq 1 30); do [ "$(docker exec $R2 gobgp neighbor | grep -c Establ)" = 2 ] && return; sleep 1; done; echo "sessions did not establish"; }

echo "== start: empty lab tables, veloced replaces gobgpd on r2"
SKIP_BASELINE=1 ./ab.sh fibdemo $GOBGPD >/dev/null 2>&1
docker exec $R2 sh -c 'pkill gobgpd; for i in $(seq 1 20); do alive=0; for p in $(pgrep -x gobgpd); do s=$(awk "{print \$3}" /proc/$p/stat 2>/dev/null); [ -n "$s" ] && [ "$s" != Z ] && alive=1; done; [ $alive = 0 ] && break; sleep 1; done'
docker cp "$VELOCED" $R2:/usr/local/bin/veloced
start_veloced; add_peers

docker exec $R1 sh -c 'gobgp global rib add 192.0.2.0/24 -a ipv4; gobgp global rib add 198.51.100.0/24 -a ipv4; gobgp global rib add 10.0.12.0/30 -a ipv4; gobgp global rib add 100.64.0.0/24 nexthop 172.31.255.1 -a ipv4'
docker exec $R3 sh -c 'gobgp global rib add 203.0.113.0/24 -a ipv4'
sleep 3
check "learned prefixes appear in ip route (proto 201, metric 20)" 'ip route show proto 201' '10.0.12.0/30 via 10.0.12.1 dev eth1 metric 20 
192.0.2.0/24 via 10.0.12.1 dev eth1 metric 20 
198.51.100.0/24 via 10.0.12.1 dev eth1 metric 20 
203.0.113.0/24 via 10.0.23.2 dev eth2 metric 20 '
check "kernel connected route for an overlapping prefix is untouched" 'ip route show 10.0.12.0/30 proto kernel' '10.0.12.0/30 dev eth1 scope link src 10.0.12.2 '
check "prefix with an unreachable next hop is not installed" 'ip route show 100.64.0.0/24 | wc -l' '0'
check "unreachable next hop was logged once as a warning" 'grep -c "next hop not reachable" /var/log/veloced.log' '1'

docker exec $R1 sh -c 'gobgp global rib del 198.51.100.0/24 -a ipv4; gobgp global rib del 10.0.12.0/30 -a ipv4'; sleep 2
check "withdrawn prefixes are removed from ip route" 'ip route show proto 201' '192.0.2.0/24 via 10.0.12.1 dev eth1 metric 20 
203.0.113.0/24 via 10.0.23.2 dev eth2 metric 20 '
check "kernel connected route still there after the overlapping prefix was withdrawn" 'ip route show 10.0.12.0/30' '10.0.12.0/30 dev eth1 proto kernel scope link src 10.0.12.2 '

docker exec $R2 sh -c 'kill -TERM $(pgrep -x veloced)'; sleep 3
check "clean stop (SIGTERM) removes every route veloced installed" 'ip route show proto 201 | wc -l' '0'

start_veloced; add_peers; sleep 2
check "routes are relearned after restart" 'ip route show proto 201 | wc -l' '2'
docker exec $R2 sh -c 'kill -9 $(pgrep -x veloced)'; sleep 1
check "kill -9 leaves stale routes (expected)" 'ip route show proto 201 | wc -l' '2'
start_veloced   # no peers added: nothing can be relearned
check "stale routes from the crash are removed on the next start" 'ip route show proto 201 | wc -l' '0'
check "the kernel's own routes survive everything" 'ip route show proto kernel | wc -l' '3'

echo "== restore: stop veloced, restart gobgpd on all routers"
docker exec $R2 sh -c 'kill -TERM $(pgrep -x veloced); sleep 3; rm -f /usr/local/bin/veloced'
SKIP_BASELINE=1 ./ab.sh restore $GOBGPD >/dev/null 2>&1; sleep 3
docker exec $R2 gobgp neighbor
echo "== $pass passed, $fail failed"
[ $fail = 0 ]
