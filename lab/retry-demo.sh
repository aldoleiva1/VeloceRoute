#!/bin/bash
# Live test of next hop retry, kernel-drop recovery and link-local binding (task E4-2) in the 3-router lab.
# Usage: ./retry-demo.sh <linux-veloced-binary> [gobgpd-binary-for-restore]
# r2 runs veloced -fib-sync, peered with r1. r1 advertises prefixes whose next hops are on a dummy network that r2
# gets only later, and r2's dummy interface is flapped. Then the r1-r2 session is replaced by an unnumbered
# (link-local) one and an IPv6 prefix must be installed bound to eth1. Restores the lab afterwards.
set -u
VELOCED=${1:?usage: ./retry-demo.sh <linux-veloced-binary> [gobgpd]}; GOBGPD=${2:-gobgpd-decode}
R1=clab-gobgp3-r1; R2=clab-gobgp3-r2; R3=clab-gobgp3-r3
pass=0; fail=0
check() { # check <description> <command run on r2> <expected output>
  got=$(docker exec $R2 sh -c "$2" 2>&1); if [ "$got" = "$3" ]; then echo "PASS  $1"; pass=$((pass+1)); else echo "FAIL  $1"; echo "      want: $3"; echo "      got:  $got"; fail=$((fail+1)); fi; }
settle() { sleep "${1:-2}"; }
r() { docker exec $R2 sh -c "$1"; }

echo "== start: empty lab tables, veloced replaces gobgpd on r2 (r1 peer only)"
SKIP_BASELINE=1 ./ab.sh retrydemo $GOBGPD >/dev/null 2>&1
r 'pkill gobgpd; for i in $(seq 1 20); do alive=0; for p in $(pgrep -x gobgpd); do s=$(awk "{print \$3}" /proc/$p/stat 2>/dev/null); [ -n "$s" ] && [ "$s" != Z ] && alive=1; done; [ $alive = 0 ] && break; sleep 1; done'
docker cp "$VELOCED" $R2:/usr/local/bin/veloced
docker exec -d $R2 sh -c 'veloced -asn 65002 -router-id 2.2.2.2 -fib-sync -fib-retry-interval 0 -grpc-address 127.0.0.1:50051 > /var/log/veloced.log 2>&1'; sleep 2
r 'gobgp neighbor add 10.0.12.1 as 65001' >/dev/null
for i in $(seq 1 30); do [ "$(r 'gobgp neighbor | grep -c Establ')" = 1 ] && break; sleep 1; done

docker exec $R1 sh -c 'gobgp global rib add 192.0.2.0/24 -a ipv4
 gobgp global rib add 198.51.100.0/24 nexthop 10.0.88.9 -a ipv4
 gobgp global rib add 100.64.0.0/24 nexthop 10.0.77.9 -a ipv4'
settle 3
check "a normal prefix is installed" 'ip route show 192.0.2.0/24 proto 201 | wc -l' '1'
check "a prefix with a next hop on a network we do not have is not installed" 'ip route show 198.51.100.0/24 | wc -l' '0'
check "each of the two was logged once, with the promise to retry" 'grep -c "will retry when the network changes" /var/log/veloced.log' '2'

echo "== the next hop network appears"
r 'ip link add dum0 type dummy; ip addr add 10.0.88.1/24 dev dum0; ip link set dum0 up'; settle
check "the waiting route is installed by the retry" 'ip route show 198.51.100.0/24 proto 201 | grep -o "via [0-9.]* dev [a-z0-9]*"' 'via 10.0.88.9 dev dum0'
check "the retry was logged" 'grep -c "installed after its next hop became reachable" /var/log/veloced.log' '1'
check "the other waiting prefix (no network yet) is still not installed" 'ip route show 100.64.0.0/24 | wc -l' '0'

echo "== the address under the next hop is removed and returns (the kernel drops the route silently)"
r 'ip addr del 10.0.88.1/24 dev dum0'; settle
check "the route is gone while its next hop is unreachable" 'ip route show 198.51.100.0/24 | wc -l' '0'
check "veloced noticed the kernel had dropped it" 'grep -c "removed or changed outside veloced" /var/log/veloced.log' '1'
check "the unrelated route was not disturbed" 'ip route show 192.0.2.0/24 proto 201 | wc -l' '1'
r 'ip addr add 10.0.88.1/24 dev dum0'; settle
check "the route returns with the address" 'ip route show 198.51.100.0/24 proto 201 | grep -o "via [0-9.]* dev [a-z0-9]*"' 'via 10.0.88.9 dev dum0'

echo "== link flap"
r 'ip link set dum0 down'; settle
check "link down: route is gone" 'ip route show 198.51.100.0/24 | wc -l' '0'
r 'ip link set dum0 up'; settle
check "link up: route is back" 'ip route show 198.51.100.0/24 proto 201 | wc -l' '1'

echo "== a withdrawal cancels a waiting route"
docker exec $R1 sh -c 'gobgp global rib del 100.64.0.0/24 -a ipv4'; settle
r 'ip addr add 10.0.77.1/24 dev dum0'; settle
check "a prefix withdrawn while waiting is not installed when its network appears" 'ip route show 100.64.0.0/24 | wc -l' '0'

echo "== clean stop"
r 'kill -TERM $(pgrep -x veloced)'; sleep 3
check "SIGTERM removes every route veloced installed" 'ip route show proto 201 | wc -l' '0'

echo "== link-local next hop: replace the r1 session by an unnumbered one"
docker exec -d $R2 sh -c 'veloced -asn 65002 -router-id 2.2.2.2 -fib-sync -fib-retry-interval 0 -grpc-address 127.0.0.1:50051 > /var/log/veloced.log 2>&1'; sleep 2
IF1=$(docker exec $R1 sh -c "ip -o -4 addr show | grep 10.0.12.1/ | awk '{print \$2}'"); IF2=$(r "ip -o -4 addr show | grep 10.0.12.2/ | awk '{print \$2}'")
echo "   r1 faces r2 on $IF1, r2 faces r1 on $IF2"
# the gobgp CLI finds the peer's link-local address in the kernel neighbor table, which is empty until the link has carried IPv6
docker exec $R1 sh -c "ping -6 -c 2 -I $IF1 ff02::1" >/dev/null 2>&1; r "ping -6 -c 2 -I $IF2 ff02::1" >/dev/null 2>&1
docker exec $R1 sh -c "gobgp neighbor del 10.0.12.2 >/dev/null 2>&1; gobgp neighbor add interface $IF1 as 65002" >/dev/null
r "gobgp neighbor add interface $IF2 as 65001" >/dev/null
for i in $(seq 1 40); do [ "$(r 'gobgp neighbor | grep -c Establ')" = 1 ] && break; sleep 1; done
docker exec $R1 sh -c 'gobgp global rib add 2001:db8:1::/48 -a ipv6'; settle 3
check "the IPv6 prefix learned over the unnumbered session is installed" 'ip -6 route show 2001:db8:1::/48 proto 201 | wc -l' '1'
check "its next hop is a link-local address bound to the session interface" "ip -6 route show 2001:db8:1::/48 proto 201 | grep -c \"via fe80::.* dev $IF2\"" '1'
r 'kill -TERM $(pgrep -x veloced); sleep 3; rm -f /usr/local/bin/veloced; ip link del dum0 2>/dev/null; true'

echo "== restore: restart gobgpd on all routers"
SKIP_BASELINE=1 ./ab.sh restore $GOBGPD >/dev/null 2>&1; settle 3
r 'gobgp neighbor'
echo "== $pass passed, $fail failed"
[ $fail = 0 ]
