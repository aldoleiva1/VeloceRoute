#!/bin/bash
# Interleaved runs of one binary under several environment settings.
# Usage: ./run-matrix.sh <binary> <runs> "<label>|<ENV settings>" ...
# Example: ./run-matrix.sh gobgpd-decode 3 "gc-default|" "gc200|GOGC=200"
# FEEDGEN_ARGS is passed through to baseline.sh.
BIN=$1; RUNS=$2; shift 2
for i in $(seq 1 "$RUNS"); do
  for spec in "$@"; do
    l=${spec%%|*}; e=${spec#*|}
    echo "== $l run$i ($e)"; GOBGPD_ENV="$e" ./ab.sh $l-run$i $BIN || echo "FAILED $l-run$i"
  done
done
echo ALL-DONE
