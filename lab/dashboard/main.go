// dashboard scrapes the Prometheus/pprof endpoint of each gobgpd in the lab,
// keeps a short history, and serves a single-page UI. It can also trigger the
// synthetic feed injector for demos. Stdlib only.
package main

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed index.html
var indexHTML []byte

const histLen = 300 // samples (1/s)

type Peer struct {
	Addr       string  `json:"addr"`
	ASN        float64 `json:"asn"`
	State      int     `json:"state"`
	Uptime     float64 `json:"uptime"`
	Received   float64 `json:"received"`
	Accepted   float64 `json:"accepted"`
	Advertised float64 `json:"advertised"`
	OutQueue   float64 `json:"outQueue"`
	UpdatesIn  float64 `json:"updatesIn"`
	UpdatesOut float64 `json:"updatesOut"`
	Flaps      float64 `json:"flaps"`
}

type Sample struct {
	T          int64   `json:"t"`
	HeapAlloc  float64 `json:"heapAlloc"`
	HeapInuse  float64 `json:"heapInuse"`
	RSS        float64 `json:"rss"`
	Goroutines float64 `json:"goroutines"`
	GCCount    float64 `json:"gcCount"`
	GCPauseSum float64 `json:"gcPauseSum"`
	Mallocs    float64 `json:"mallocs"`
	AllocTotal float64 `json:"allocTotal"`
	CPUSeconds float64 `json:"cpuSeconds"`
	// derived rates
	MallocsPS float64 `json:"mallocsPS"`
	AllocMBPS float64 `json:"allocMBPS"`
	CPUPct    float64 `json:"cpuPct"`
	GCPS      float64 `json:"gcPS"`
	Accepted  float64 `json:"accepted"`
	UpdatesPS float64 `json:"updatesPS"`
	updatesIn float64
}

type Router struct {
	Name    string   `json:"name"`
	Addr    string   `json:"addr"`
	Up      bool     `json:"up"`
	Err     string   `json:"err,omitempty"`
	Peers   []Peer   `json:"peers"`
	Latest  Sample   `json:"latest"`
	History []Sample `json:"history"`
}

type Job struct {
	Running bool   `json:"running"`
	Action  string `json:"action"`
	N       int    `json:"n"`
	Output  string `json:"output"`
	Started int64  `json:"started"`
}

var (
	mu      sync.Mutex
	routers []*Router
	job     Job
)

func main() {
	spec := env("ROUTERS", "r1=10.250.0.11,r2=10.250.0.12,r3=10.250.0.13")
	for _, kv := range strings.Split(spec, ",") {
		p := strings.SplitN(kv, "=", 2)
		if len(p) == 2 {
			routers = append(routers, &Router{Name: p[0], Addr: p[1], Peers: []Peer{}})
		}
	}
	go pollLoop()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	http.HandleFunc("/api/state", handleState)
	http.HandleFunc("/api/feed", handleFeed)
	listen := env("LISTEN", ":8080")
	log.Printf("dashboard on %s, routers=%s", listen, spec)
	log.Fatal(http.ListenAndServe(listen, nil))
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func pollLoop() {
	t := time.NewTicker(time.Second)
	client := &http.Client{Timeout: 900 * time.Millisecond}
	for range t.C {
		var wg sync.WaitGroup
		for _, r := range routers {
			wg.Add(1)
			go func(r *Router) { defer wg.Done(); scrape(client, r) }(r)
		}
		wg.Wait()
	}
}

func scrape(c *http.Client, r *Router) {
	resp, err := c.Get("http://" + r.Addr + ":6060/metrics")
	if err != nil {
		mu.Lock()
		r.Up, r.Err = false, err.Error()
		mu.Unlock()
		return
	}
	defer resp.Body.Close()
	s := Sample{T: time.Now().UnixMilli()}
	peers := map[string]*Peer{}
	peer := func(l map[string]string) *Peer {
		a := l["peer"]
		if peers[a] == nil {
			peers[a] = &Peer{Addr: a}
		}
		return peers[a]
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		name, labels, val, ok := parseLine(line)
		if !ok {
			continue
		}
		switch name {
		case "go_memstats_alloc_bytes":
			s.HeapAlloc = val
		case "go_memstats_heap_inuse_bytes":
			s.HeapInuse = val
		case "process_resident_memory_bytes":
			s.RSS = val
		case "go_goroutines":
			s.Goroutines = val
		case "go_gc_duration_seconds_count":
			s.GCCount = val
		case "go_gc_duration_seconds_sum":
			s.GCPauseSum = val
		case "go_memstats_mallocs_total":
			s.Mallocs = val
		case "go_memstats_alloc_bytes_total":
			s.AllocTotal = val
		case "process_cpu_seconds_total":
			s.CPUSeconds = val
		case "bgp_peer_session_state":
			peer(labels).State = int(val)
		case "bgp_peer_asn":
			peer(labels).ASN = val
		case "bgp_peer_uptime":
			peer(labels).Uptime = val
		case "bgp_peer_out_queue_count":
			peer(labels).OutQueue = val
		case "bgp_peer_flop_count":
			peer(labels).Flaps = val
		case "bgp_routes_received":
			peer(labels).Received += val
		case "bgp_routes_accepted":
			peer(labels).Accepted += val
		case "bgp_routes_advertised":
			peer(labels).Advertised += val
		case "bgp_received_update_total":
			peer(labels).UpdatesIn += val
		case "bgp_sent_update_total":
			peer(labels).UpdatesOut += val
		}
	}
	pl := make([]Peer, 0, len(peers))
	for _, p := range peers {
		s.Accepted += p.Accepted
		s.updatesIn += p.UpdatesIn
		pl = append(pl, *p)
	}
	sort.Slice(pl, func(i, j int) bool { return pl[i].Addr < pl[j].Addr })

	mu.Lock()
	defer mu.Unlock()
	if n := len(r.History); n > 0 {
		prev := r.History[n-1]
		if dt := float64(s.T-prev.T) / 1000; dt > 0 {
			s.MallocsPS = nonneg(s.Mallocs-prev.Mallocs) / dt
			s.AllocMBPS = nonneg(s.AllocTotal-prev.AllocTotal) / dt / 1e6
			s.CPUPct = nonneg(s.CPUSeconds-prev.CPUSeconds) / dt * 100
			s.GCPS = nonneg(s.GCCount-prev.GCCount) / dt
			s.UpdatesPS = nonneg(s.updatesIn-prev.updatesIn) / dt
		}
	}
	r.Up, r.Err, r.Peers, r.Latest = true, "", pl, s
	r.History = append(r.History, s)
	if len(r.History) > histLen {
		r.History = r.History[len(r.History)-histLen:]
	}
}

func nonneg(f float64) float64 {
	if f < 0 {
		return 0
	}
	return f
}

// parseLine handles `name{a="b",c="d"} 1.5` and `name 1.5`.
func parseLine(line string) (string, map[string]string, float64, bool) {
	sp := strings.LastIndexByte(line, ' ')
	if sp < 0 {
		return "", nil, 0, false
	}
	val, err := strconv.ParseFloat(line[sp+1:], 64)
	if err != nil {
		return "", nil, 0, false
	}
	head := line[:sp]
	i := strings.IndexByte(head, '{')
	if i < 0 {
		return head, nil, val, true
	}
	labels := map[string]string{}
	body := strings.TrimSuffix(head[i+1:], "}")
	for len(body) > 0 {
		eq := strings.Index(body, `="`)
		if eq < 0 {
			break
		}
		key := body[:eq]
		rest := body[eq+2:]
		end := strings.Index(rest, `"`)
		if end < 0 {
			break
		}
		labels[strings.TrimPrefix(key, ",")] = rest[:end]
		body = strings.TrimPrefix(rest[end+1:], ",")
	}
	return head[:i], labels, val, true
}

func handleState(w http.ResponseWriter, _ *http.Request) {
	mu.Lock()
	defer mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"routers": routers, "job": job, "now": time.Now().UnixMilli()})
}

// POST /api/feed?action=inject|withdraw&n=900000 runs feedgen against the source router.
func handleFeed(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	action := r.URL.Query().Get("action")
	n, err := strconv.Atoi(r.URL.Query().Get("n"))
	if (action != "inject" && action != "withdraw") || err != nil || n < 1 || n > 2000000 {
		http.Error(w, "bad action or n (1..2000000)", http.StatusBadRequest)
		return
	}
	mu.Lock()
	if job.Running {
		mu.Unlock()
		http.Error(w, "a feed job is already running", http.StatusConflict)
		return
	}
	job = Job{Running: true, Action: action, N: n, Started: time.Now().UnixMilli()}
	mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		args := []string{"-addr", env("FEED_ADDR", "10.250.0.11:50051"), "-n", strconv.Itoa(n)}
		if action == "withdraw" {
			args = append(args, "-withdraw")
		}
		out, err := exec.CommandContext(ctx, env("FEEDGEN", "/usr/local/bin/feedgen"), args...).CombinedOutput()
		res := strings.TrimSpace(string(out))
		if err != nil {
			res = fmt.Sprintf("%s (error: %v)", res, err)
		}
		mu.Lock()
		job.Running, job.Output = false, res
		mu.Unlock()
	}()
	w.WriteHeader(http.StatusAccepted)
}
