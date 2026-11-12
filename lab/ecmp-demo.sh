#!/bin/bash
# Live test of ECMP kernel routes (task E4-3) in the 3-router lab.
# Usage: ./ecmp-demo.sh <linux-veloced-binary> [gobgpd-binary-for-restore]
# r1 and r3 both advertise the same prefixes to r2. veloced -fib-sync -multipath on r2 must install ONE kernel
# route with BOTH next hops, shrink it when a path is withdrawn or a session goes down, grow it again, and
# remove it on stop. A second run without -multipath must install a single next hop. Restores the lab afterwards.
set -u
VELOCED=${1:?usage: ./ecmp-demo.sh <linux-veloced-binary> [gobgpd]}; GOBGPD=${2:-gobgpd-decode}
R1=clab-gobgp3-r1; R2=clab-gobgp3-r2; R3=clab-gobgp3-r3
pass=0; fail=0
check() { # check <description> <command run on r2> <expected output>
  got=$(docker exec $R2 sh -c "$2" 2>&1); if [ "$got" = "$3" ]; then echo "PASS  $1"; pass=$((pass+1)); else echo "FAIL  $1"; echo "      want: $3"; echo "      got:  $got"; fail=$((fail+1)); fi; }
hops() { echo "ip route show $1 proto 201 | grep -o 'via [0-9.]*' | sort | tr '\n' ' '"; }   # next hops of our route
start_veloced() { docker exec -d $R2 sh -c "veloced -asn 65002 -router-id 2.2.2.2 -fib-sync $1 -grpc-address 127.0.0.1:50051 > /var/log/veloced.log 2>&1"; sleep 2; }
stop_veloced() { docker exec $R2 sh -c 'kill -TERM $(pgrep -x veloced); sleep 3'; }
add_peers() { docker exec $R2 sh -c 'gobgp neighbor add 10.0.12.1 as 65001; gobgp neighbor add 10.0.23.2 as 65003' >/dev/null; for i in $(seq 1 30); do [ "$(docker exec $R2 gobgp neighbor | grep -c Establ)" = 2 ] && return; sleep 1; done; echo "sessions did not establish"; }
advertise() { docker exec $R1 sh -c 'gobgp global rib add 192.0.2.0/24 -a ipv4; gobgp global rib add 198.51.100.0/24 -a ipv4'
              docker exec $R3 sh -c 'gobgp global rib add 192.0.2.0/24 -a ipv4; gobgp global rib add 203.0.113.0/24 -a ipv4'; }

echo "== start: empty lab tables, veloced -multipath replaces gobgpd on r2"
SKIP_BASELINE=1 ./ab.sh ecmpdemo $GOBGPD >/dev/null 2>&1
docker exec $R2 sh -c 'pkill gobgpd; for i in $(seq 1 20); do alive=0; for p in $(pgrep -x gobgpd); do s=$(awk "{print \$3}" /proc/$p/stat 2>/dev/null); [ -n "$s" ] && [ "$s" != Z ] && alive=1; done; [ $alive = 0 ] && break; sleep 1; done'
docker cp "$VELOCED" $R2:/usr/local/bin/veloced
start_veloced -multipath; add_peers; advertise; sleep 3

check "a prefix both routers advertise gets one route with both next hops" "$(hops 192.0.2.0/24)" 'via 10.0.12.1 via 10.0.23.2 '
check "it is a single kernel route, not two" 'ip route show 192.0.2.0/24 proto 201 | grep -c "^192.0.2.0/24"' '1'
check "the route carries our protocol and metric" 'ip route show 192.0.2.0/24 | head -1' '192.0.2.0/24 proto 201 metric 20 '
check "a prefix only r1 advertises has one next hop" "$(hops 198.51.100.0/24)" 'via 10.0.12.1 '
check "a prefix only r3 advertises has one next hop" "$(hops 203.0.113.0/24)" 'via 10.0.23.2 '

docker exec $R1 sh -c 'gobgp global rib del 192.0.2.0/24 -a ipv4'; sleep 2
check "r1 withdraws: the route shrinks to r3's next hop" "$(hops 192.0.2.0/24)" 'via 10.0.23.2 '
docker exec $R1 sh -c 'gobgp global rib add 192.0.2.0/24 -a ipv4'; sleep 2
check "r1 advertises again: both next hops are back" "$(hops 192.0.2.0/24)" 'via 10.0.12.1 via 10.0.23.2 '

docker exec $R2 gobgp neighbor 10.0.23.2 disable >/dev/null; sleep 3
check "session to r3 goes down: the route shrinks to r1's next hop" "$(hops 192.0.2.0/24)" 'via 10.0.12.1 '
check "prefixes only r3 had are removed" 'ip route show 203.0.113.0/24 proto 201 | wc -l' '0'
docker exec $R2 gobgp neighbor 10.0.23.2 enable >/dev/null; sleep 6
check "session to r3 comes back: both next hops again" "$(hops 192.0.2.0/24)" 'via 10.0.12.1 via 10.0.23.2 '

stop_veloced
check "clean stop removes the ECMP routes too" 'ip route show proto 201 | wc -l' '0'

echo "== without -multipath the same paths give one next hop"
start_veloced ""; add_peers; sleep 3
check "single next hop when -multipath is off" 'ip route show 192.0.2.0/24 proto 201 | grep -c "via"' '1'
stop_veloced

echo "== crash recovery with an ECMP route"
start_veloced -multipath; add_peers; sleep 3
docker exec $R2 sh -c 'kill -9 $(pgrep -x veloced)'; sleep 1
check "kill -9 leaves the ECMP route (expected)" "$(hops 192.0.2.0/24)" 'via 10.0.12.1 via 10.0.23.2 '
start_veloced -multipath
check "the stale ECMP route is removed on the next start" 'ip route show proto 201 | wc -l' '0'
check "the kernel's own routes survive everything" 'ip route show proto kernel | wc -l' '3'

echo "== restore: stop veloced, restart gobgpd on all routers"
docker exec $R2 sh -c 'kill -TERM $(pgrep -x veloced); sleep 3; rm -f /usr/local/bin/veloced'
SKIP_BASELINE=1 ./ab.sh restore $GOBGPD >/dev/null 2>&1; sleep 3
docker exec $R2 gobgp neighbor
echo "== $pass passed, $fail failed"
[ $fail = 0 ]
