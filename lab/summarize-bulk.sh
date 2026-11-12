#!/bin/bash
# Table of fib-bulk.sh results: ./summarize-bulk.sh results/bulk-before-r*.txt ...
# Columns: bgp = BGP converged on r2, kernel = kernel table complete, lag = kernel after bgp, cpu = veloced CPU seconds,
# rss = peak resident MB, stop = seconds for SIGTERM to remove every route.
printf "%-34s %7s %7s %6s %6s %6s %6s\n" run bgp_s kernel_s lag_s cpu_s rss_mb stop_s
for f in "$@"; do
  v() { grep -E "^$1=" "$f" | head -1 | sed -E "s/^$1=//; s/ .*//"; }
  stop=$(grep -oE "stop_seconds=[0-9.]+" "$f" | head -1 | cut -d= -f2)
  printf "%-34s %7.2f %7s %6s %6.1f %6s %6.1f\n" "$(basename "$f" | sed -E 's/-[0-9]{8}-[0-9]{6}\.txt//')" \
    "$(v bgp_converged_r2_seconds)" "$(v kernel_programmed_seconds | cut -c1-5)" "$(v kernel_lag_after_bgp_seconds | cut -c1-4)" \
    "$(v veloced_cpu_seconds)" "$(v veloced_peak_rss_mb)" "${stop:-0}"
done
