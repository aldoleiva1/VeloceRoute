// Copyright (C) 2026 VeloceRoute contributors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/osrg/gobgp/v4/internal/pkg/fib"
)

// config is the whole configuration of the skeleton: just enough to start the
// BGP engine and its gRPC API. A real config file schema is task E2-2.
type config struct {
	ASN        uint32
	RouterID   string
	ListenPort int32  // BGP listen port; -1 disables the listener
	GrpcAddr   string // gRPC API address; localhost by default (task E8-1)
	LogLevel   string
	LogFormat  string
	Version    bool

	// Multipath makes BGP keep every equal-cost path (use-multiple-paths) so
	// kernel sync can build ECMP routes (task E4-3).
	Multipath bool

	// Linux kernel route sync (task E4-1).
	FibSync     bool
	FibProtocol int // rtnetlink protocol id stamped on our routes
	FibTable    int // routing table to program
	FibMetric   int // route priority; see fib.KernelOptions.Metric
	FibMaxPaths int // most next hops in one ECMP route (with -multipath)
	// FibRetry is how often prefixes whose next hop is not reachable are
	// retried even if no network change is seen; 0 turns the timer off.
	FibRetry time.Duration
	// FibStats logs the kernel sync counters this often (0: never).
	FibStats time.Duration
	// PprofAddr serves Go's profiling endpoints (empty: off). It exposes
	// internals, so bind it to localhost.
	PprofAddr string
}

func parseFlags(args []string, errOut io.Writer) (*config, error) {
	c := &config{}
	fs := flag.NewFlagSet("veloced", flag.ContinueOnError)
	fs.SetOutput(errOut)
	asn := fs.Uint("asn", 0, "local AS number (required)")
	fs.StringVar(&c.RouterID, "router-id", "", "BGP router ID, an IPv4 address (required)")
	listen := fs.Int("listen-port", 179, "TCP port for incoming BGP sessions; -1 disables the listener")
	fs.StringVar(&c.GrpcAddr, "grpc-address", "127.0.0.1:50051", "gRPC API listen address (host:port or unix://path)")
	fs.StringVar(&c.LogLevel, "log-level", "info", "log level: debug, info, warn, error")
	fs.StringVar(&c.LogFormat, "log-format", "text", "log format: text or json")
	fs.BoolVar(&c.Version, "version", false, "print the version and exit")
	fs.BoolVar(&c.FibSync, "fib-sync", false, "copy BGP best paths into the Linux kernel routing table (Linux only; needs CAP_NET_ADMIN)")
	fs.IntVar(&c.FibProtocol, "fib-protocol", fib.DefaultProtocol, "route protocol id that marks the routes veloced owns (5 to 255)")
	fs.IntVar(&c.FibTable, "fib-table", fib.DefaultTable, "routing table to program (254 is the main table)")
	fs.IntVar(&c.FibMetric, "fib-metric", fib.DefaultMetric, "route metric; kept non-zero so a BGP route cannot replace a connected or static one")
	fs.BoolVar(&c.Multipath, "multipath", false, "keep every equal-cost BGP path; with -fib-sync the kernel route gets all their next hops (ECMP)")
	fs.IntVar(&c.FibMaxPaths, "fib-max-paths", fib.DefaultMaxNextHops, "most next hops in one ECMP kernel route (with -multipath and -fib-sync)")
	fs.DurationVar(&c.FibRetry, "fib-retry-interval", fib.DefaultRetryInterval, "how often to retry routes whose next hop was not reachable, besides on network changes (0 turns the timer off)")
	fs.DurationVar(&c.FibStats, "fib-stats-interval", 0, "log kernel sync counters this often, for example 10s (0 turns it off)")
	fs.StringVar(&c.PprofAddr, "pprof-address", "", "serve Go profiling endpoints on this address, for example 127.0.0.1:6060 (empty: off)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if *asn > 0xFFFFFFFF {
		return nil, fmt.Errorf("-asn %d does not fit in 32 bits", *asn)
	}
	c.ASN = uint32(*asn)
	c.ListenPort = int32(*listen)
	if c.Version {
		return c, nil
	}
	return c, c.validate()
}

func (c *config) validate() error {
	var errs []error
	if c.ASN == 0 {
		errs = append(errs, errors.New("-asn is required and must be non-zero"))
	}
	if addr, err := netip.ParseAddr(c.RouterID); err != nil || !addr.Is4() {
		errs = append(errs, fmt.Errorf("-router-id %q must be an IPv4 address", c.RouterID))
	}
	if c.ListenPort < -1 || c.ListenPort > 65535 {
		errs = append(errs, fmt.Errorf("-listen-port %d is out of range (-1 to 65535)", c.ListenPort))
	}
	if c.FibStats < 0 {
		errs = append(errs, fmt.Errorf("-fib-stats-interval %s must not be negative", c.FibStats))
	}
	if c.GrpcAddr == "" {
		errs = append(errs, errors.New("-grpc-address must not be empty"))
	}
	if _, err := parseLevel(c.LogLevel); err != nil {
		errs = append(errs, err)
	}
	if c.LogFormat != "text" && c.LogFormat != "json" {
		errs = append(errs, fmt.Errorf("-log-format %q must be text or json", c.LogFormat))
	}
	if c.FibSync {
		if runtime.GOOS != "linux" {
			errs = append(errs, fib.ErrKernelUnsupported)
		}
		if c.FibProtocol < 5 || c.FibProtocol > 255 {
			errs = append(errs, fmt.Errorf("-fib-protocol %d must be 5 to 255 (0 to 4 are the kernel's own)", c.FibProtocol))
		}
		if c.FibTable < 1 || c.FibTable == 255 {
			errs = append(errs, fmt.Errorf("-fib-table %d is not usable (255 is the local table)", c.FibTable))
		}
		if c.FibMetric < 1 {
			errs = append(errs, fmt.Errorf("-fib-metric %d must be at least 1", c.FibMetric))
		}
		if c.FibRetry < 0 {
			errs = append(errs, fmt.Errorf("-fib-retry-interval %s must not be negative", c.FibRetry))
		}
		if c.FibMaxPaths < 1 {
			errs = append(errs, fmt.Errorf("-fib-max-paths %d must be at least 1", c.FibMaxPaths))
		}
	}
	return errors.Join(errs...)
}

func parseLevel(s string) (slog.Level, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return 0, fmt.Errorf("-log-level %q must be debug, info, warn or error", s)
	}
	return l, nil
}

// newLogger builds the structured logger and the level variable the BGP
// server uses, so the level can be changed at run time.
func newLogger(c *config, out io.Writer) (*slog.Logger, *slog.LevelVar) {
	lvl := new(slog.LevelVar)
	l, _ := parseLevel(c.LogLevel)
	lvl.Set(l)
	opts := &slog.HandlerOptions{Level: lvl}
	if c.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(out, opts)), lvl
	}
	return slog.New(slog.NewTextHandler(out, opts)), lvl
}

func stderr() io.Writer { return os.Stderr }
