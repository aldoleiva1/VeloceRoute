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
