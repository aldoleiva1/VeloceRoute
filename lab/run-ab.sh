#!/bin/bash
# Interleaved A/B: upstream vs branch, N runs each. Usage: ./run-ab.sh [runs]
RUNS=${1:-3}
for i in $(seq 1 "$RUNS"); do
  echo "== upstream run$i"; ./ab.sh upstream-run$i gobgpd-upstream || echo "FAILED upstream-run$i"
  echo "== branch run$i";   ./ab.sh branch-run$i   gobgpd-branch   || echo "FAILED branch-run$i"
done
echo ALL-DONE
