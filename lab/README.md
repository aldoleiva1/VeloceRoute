# GoBGP 3-router lab

Topology: r1 (AS65001) - r2 (AS65002) - r3 (AS65003), eBGP, ECMP enabled. r2 is the device under test.

    # (re)build binaries for the lab (from the repo root), then image
    (cd .. && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o lab/gobgpd ./cmd/gobgpd && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o lab/gobgp ./cmd/gobgp)
    docker build -t gobgp-lab:local .

    # deploy / destroy (containerlab runs from its container image)
    CL='docker run --rm --privileged --network host --pid host -v /var/run/docker.sock:/var/run/docker.sock -v /var/run/netns:/var/run/netns -v /var/lib/docker/containers:/var/lib/docker/containers -v '"$PWD:$PWD"' -w '"$PWD"' ghcr.io/srl-labs/clab containerlab'
    $CL deploy -t topo.clab.yml --reconfigure      # destroy -t topo.clab.yml --cleanup

    # dashboard on http://localhost:8080 (feedgen + dashboard built into the image)
    (cd tools && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o ../feedgen-linux ./feedgen)
    (cd dashboard && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o ../dashboard-linux .)
    docker build -f Dockerfile.dashboard -t gobgp-dashboard:local .
    docker run -d --name gobgp-dashboard --network gobgp3-mgmt -p 8080:8080 gobgp-dashboard:local

    # baseline: inject N /24s on r1, wait for r2 and r3, save runtime metrics to results/
    docker cp feedgen-linux clab-gobgp3-r1:/usr/local/bin/feedgen && ./baseline.sh 900000 baseline-900k

Management IPs are pinned to 10.250.0.11-13. gobgpd serves Prometheus and pprof on :6060 in each router.

## Comparing two builds (A/B)

    # build linux binaries, e.g. lab/gobgpd-a and lab/gobgpd-b, then interleave runs with fresh daemons each time
    ./run-ab2.sh a gobgpd-a b gobgpd-b 3
    python3 summarize.py a b          # mean [min .. max] of allocs/route, GC cycles, CPU, converge time on r2

`ab.sh` restarts gobgpd on r1-r3 with the given binary, waits for the old process to exit and for both sessions to establish, then runs `baseline.sh`. Results go to `results/<label>-run<N>-<timestamp>.txt`. Allocation counts per route are deterministic; CPU and convergence time vary 10-15% run to run on a laptop, so use at least 3 runs. See `../docs/veloceroute/performance.md` for what has been measured.

By default `feedgen` gives every prefix its own AS path (one prefix per UPDATE). For a more realistic feed where about 37 prefixes share each UPDATE, set `FEEDGEN_ARGS`: `FEEDGEN_ARGS="-share 40" ./run-ab2.sh ...` (or `run-ab3.sh` for three builds). The summary then also shows `routes/UPDATE`. `baseline.sh` copies `feedgen-linux` to r1 on every run, so rebuild it after changing the generator.

## Runtime settings (GOGC / GOMEMLIMIT)

    # one build under several environment settings, interleaved
    FEEDGEN_ARGS="-share 40" ./run-matrix.sh gobgpd-decode 3 "gc-default|" "gc200|GOGC=200" "gc400|GOGC=400"
    python3 summarize.py gc-default gc200 gc400

`GOBGPD_ENV` (used by `ab.sh`) sets variables in the environment of every gobgpd. `summarize.py` also reports `Go sys MB` (memory the Go runtime obtained from the OS, close to peak) and `routes/UPDATE`.
