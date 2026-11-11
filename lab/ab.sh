#!/bin/bash
# A/B load test with fresh daemons per run.
# Usage: ./ab.sh <label> <path-to-linux-gobgpd> [N]
# Restarts gobgpd on r1-r3 with the given binary, waits for sessions, runs baseline.sh.
set -e
LABEL=$1; BIN=$2; N=${3:-900000}
[ -f "$BIN" ] || { echo "no such binary: $BIN"; exit 1; }
# Stop the old daemons and WAIT until they are gone: a daemon holding a full table
# takes many seconds to shut down, and a new one cannot bind :179/:50051 until then.
for r in r1 r2 r3; do
  docker exec clab-gobgp3-$r sh -c 'pkill gobgpd || true' >/dev/null 2>&1
done
# alive: true if a non-zombie gobgpd exists (killed daemons linger as zombies because
# the container's PID 1 is `sleep` and never reaps them).
ALIVE='for p in $(pgrep -x gobgpd); do s=$(awk "{print \$3}" /proc/$p/stat 2>/dev/null); [ -n "$s" ] && [ "$s" != Z ] && exit 0; done; exit 1'
for r in r1 r2 r3; do
  for i in $(seq 1 60); do
    docker exec clab-gobgp3-$r sh -c "$ALIVE" || break
    [ "$i" = 30 ] && docker exec clab-gobgp3-$r sh -c 'pkill -9 gobgpd || true' >/dev/null 2>&1
    sleep 1
  done
  if docker exec clab-gobgp3-$r sh -c "$ALIVE"; then echo "gobgpd still running on $r"; exit 1; fi
done
for r in r1 r2 r3; do
  docker cp "$BIN" clab-gobgp3-$r:/usr/local/bin/gobgpd
  docker exec -d clab-gobgp3-$r sh -c 'gobgpd -f /etc/gobgp.toml --api-hosts 0.0.0.0:50051 --pprof-host 0.0.0.0:6060 > /var/log/gobgpd.log 2>&1'
done
for i in $(seq 1 60); do
  e=$(docker exec clab-gobgp3-r2 gobgp neighbor 2>/dev/null | grep -c Establ || true)
  [ "$e" = "2" ] && break; sleep 1
done
[ "$e" = "2" ] || { echo "sessions did not establish"; exit 1; }
sleep 3
./baseline.sh "$N" "$LABEL" 2>&1 | grep -E 'sent=|converged|saved'
