#!/bin/bash
# Interleaved comparison of three binaries.
# Usage: ./run-ab3.sh <labelA> <binA> <labelB> <binB> <labelC> <binC> [runs]
# FEEDGEN_ARGS (for example "-share 40") is passed through to baseline.sh.
LA=$1; BA=$2; LB=$3; BB=$4; LC=$5; BC=$6; RUNS=${7:-3}
for i in $(seq 1 "$RUNS"); do
  for pair in "$LA:$BA" "$LB:$BB" "$LC:$BC"; do
    l=${pair%%:*}; b=${pair#*:}
    echo "== $l run$i"; ./ab.sh $l-run$i $b || echo "FAILED $l-run$i"
  done
done
echo ALL-DONE
