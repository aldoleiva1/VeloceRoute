#!/bin/bash
# Interleaved A/B of two binaries. Usage: ./run-ab2.sh <labelA> <binA> <labelB> <binB> [runs]
A=$1; BA=$2; B=$3; BB=$4; RUNS=${5:-3}
for i in $(seq 1 "$RUNS"); do
  echo "== $A run$i"; ./ab.sh $A-run$i $BA || echo "FAILED $A-run$i"
  echo "== $B run$i"; ./ab.sh $B-run$i $BB || echo "FAILED $B-run$i"
done
echo ALL-DONE
