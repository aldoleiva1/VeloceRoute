#!/usr/bin/env python3
"""Summarize A/B runs: python3 summarize.py upstream branch  (labels = file prefixes in results/)."""
import re, glob, sys, statistics as st
N = 900000
def run(path):
    t = open(path).read()
    def grab(sec, r):
        m = re.search(rf'# {sec} {r}\n(.*?)(?=\n# |\nsent|\nconverged|\Z)', t, re.S)
        return {l.split()[0]: float(l.split()[1]) for l in m.group(1).strip().splitlines() if l[0] != '#'}
    b, a = grab('before', 'r2'), grab('after', 'r2')
    d = lambda k: a[k] - b[k]
    mu = re.search(r'updates_rcvd_r2_from_r1=(\d+)', t)
    res = {
        'mallocs/route': d('go_memstats_mallocs_total') / N,
        'KB/route': d('go_memstats_alloc_bytes_total') / N / 1e3,
        'GC cycles': d('go_gc_duration_seconds_count'),
        'GC pause ms': d('go_gc_duration_seconds_sum') * 1e3,
        'CPU s': d('process_cpu_seconds_total'),
        'converge s': float(re.search(r'converged_r2_r3_seconds=([\d.]+)', t).group(1)),
        'RSS MB': a['process_resident_memory_bytes'] / 1e6,
    }
    if mu:
        res['routes/UPDATE'] = N / float(mu.group(1))
    return res
labels = sys.argv[1:]
data = {l: [run(p) for p in sorted(glob.glob(f'results/{l}-run*.txt'))] for l in labels}
metrics = [m for m in next(iter(data.values()))[0] if all(m in r for runs in data.values() for r in runs)]
print(f"{'r2 metric':<15}" + ''.join(f"{l+' (n='+str(len(data[l]))+')':>34}" for l in labels) + (f"{'change':>10}" if len(labels) == 2 else ''))
for m in metrics:
    row = f"{m:<15}"; means = []
    for l in labels:
        v = [r[m] for r in data[l]]; means.append(st.mean(v))
        row += f"{st.mean(v):>12.1f}  [{min(v):.1f} .. {max(v):.1f}]".rjust(34)
    if len(labels) == 2: row += f"{(means[1]/means[0]-1)*100:>+9.0f}%"
    print(row)
