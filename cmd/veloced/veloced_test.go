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
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/osrg/gobgp/v4/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// syncBuffer is a bytes.Buffer that is safe to write from several goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestLifecycleOrder(t *testing.T) {
	var events []string
	mk := func(name string) stage {
		return stage{
			name:  name,
			start: func(context.Context) error { events = append(events, "start "+name); return nil },
			stop:  func(context.Context) error { events = append(events, "stop "+name); return nil },
		}
	}
	lc := &lifecycle{logger: discardLogger(), stages: []stage{mk("wireguard"), mk("bgp"), mk("fib")}}
	if err := lc.startAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := lc.stopAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "start wireguard,start bgp,start fib,stop fib,stop bgp,stop wireguard"
	if got := strings.Join(events, ","); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// A stage that fails to start must leave nothing running: the stages that
// already started are stopped in reverse order and the start error survives.
func TestLifecycleStartFailureUnwinds(t *testing.T) {
	var events []string
	boom := errors.New("boom")
	ok := func(name string) stage {
		return stage{name: name,
			start: func(context.Context) error { events = append(events, "start "+name); return nil },
			stop:  func(context.Context) error { events = append(events, "stop "+name); return nil }}
	}
	bad := stage{name: "bgp",
		start: func(context.Context) error { events = append(events, "start bgp"); return boom },
		stop:  func(context.Context) error { events = append(events, "stop bgp"); return nil }}
	lc := &lifecycle{logger: discardLogger(), stages: []stage{ok("wireguard"), bad, ok("fib")}}
	err := lc.startAll(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap boom", err)
	}
	want := "start wireguard,start bgp,stop wireguard"
	if got := strings.Join(events, ","); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// Every stage gets to clean up even when an earlier stop fails.
func TestLifecycleStopContinuesAfterError(t *testing.T) {
	var events []string
	fail := errors.New("stop failed")
	lc := &lifecycle{logger: discardLogger(), stages: []stage{
		{name: "a", start: func(context.Context) error { return nil }, stop: func(context.Context) error { events = append(events, "a"); return nil }},
		{name: "b", start: func(context.Context) error { return nil }, stop: func(context.Context) error { events = append(events, "b"); return fail }},
	}}
	if err := lc.startAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := lc.stopAll(context.Background())
	if !errors.Is(err, fail) || strings.Join(events, ",") != "b,a" {
		t.Fatalf("err=%v events=%v", err, events)
	}
}

func TestParseFlags(t *testing.T) {
	good := []string{"-asn", "65001", "-router-id", "10.0.0.1"}
	c, err := parseFlags(good, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.ASN != 65001 || c.RouterID != "10.0.0.1" || c.ListenPort != 179 || c.GrpcAddr != "127.0.0.1:50051" {
		t.Fatalf("defaults wrong: %+v", c)
	}
	// 4-octet ASN
	if c, err := parseFlags([]string{"-asn", "4200000001", "-router-id", "1.1.1.1"}, io.Discard); err != nil || c.ASN != 4200000001 {
		t.Fatalf("4-octet asn: %+v %v", c, err)
	}
	for name, args := range map[string][]string{
		"no asn":         {"-router-id", "10.0.0.1"},
		"no router id":   {"-asn", "65001"},
		"v6 router id":   {"-asn", "65001", "-router-id", "2001:db8::1"},
		"junk router id": {"-asn", "65001", "-router-id", "nope"},
		"asn too big":    {"-asn", "4294967296", "-router-id", "10.0.0.1"},
		"bad port":       {"-asn", "1", "-router-id", "10.0.0.1", "-listen-port", "70000"},
		"bad level":      {"-asn", "1", "-router-id", "10.0.0.1", "-log-level", "loud"},
		"bad format":     {"-asn", "1", "-router-id", "10.0.0.1", "-log-format", "xml"},
		"extra args":     {"-asn", "1", "-router-id", "10.0.0.1", "stray"},
	} {
		if _, err := parseFlags(args, io.Discard); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if c, err := parseFlags([]string{"-version"}, io.Discard); err != nil || !c.Version {
		t.Fatalf("-version should not need other flags: %v", err)
	}
}

func TestLoggerFormatAndLevel(t *testing.T) {
	var buf bytes.Buffer
	l, lvl := newLogger(&config{LogLevel: "warn", LogFormat: "json"}, &buf)
	l.Info("hidden")
	l.Warn("shown")
	lvl.Set(slog.LevelDebug)
	l.Debug("now visible")
	out := buf.String()
	if strings.Contains(out, "hidden") || !strings.Contains(out, `"msg":"shown"`) || !strings.Contains(out, "now visible") {
		t.Fatalf("unexpected log output:\n%s", out)
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// The "done when" of task E2-1: the binary starts and serves the existing gRPC
// API. This runs the real daemon (BGP listener off), queries it, then stops it.
func TestRunServesGRPCAndStopsCleanly(t *testing.T) {
	addr := freePort(t)
	cfg := &config{ASN: 65001, RouterID: "10.0.0.1", ListenPort: -1, GrpcAddr: addr, LogLevel: "warn", LogFormat: "text"}
	logs := &syncBuffer{}
	logger, lvl := newLogger(cfg, logs)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, logger, lvl) }()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := api.NewGoBgpServiceClient(conn)

	var rsp *api.GetBgpResponse
	deadline := time.Now().Add(10 * time.Second)
	for {
		rctx, rcancel := context.WithTimeout(context.Background(), time.Second)
		rsp, err = client.GetBgp(rctx, &api.GetBgpRequest{})
		rcancel()
		if err == nil && rsp.GetGlobal().GetAsn() != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("gRPC API never answered: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if g := rsp.GetGlobal(); g.GetAsn() != 65001 || g.GetRouterId() != "10.0.0.1" {
		t.Fatalf("global = %+v", g)
	}

	cancel() // what SIGTERM does
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after cancel")
	}
	// A clean start and stop must not log any warning or error.
	if out := logs.String(); out != "" {
		t.Fatalf("unexpected warnings or errors during a clean run:\n%s", out)
	}
	// The API must be gone after shutdown.
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Fatal("gRPC port still open after shutdown")
	}
}

// If the gRPC address is already taken the daemon must refuse to start, not
// run on without a management API.
func TestRunFailsFastWhenGrpcPortTaken(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	cfg := &config{ASN: 65001, RouterID: "10.0.0.1", ListenPort: -1, GrpcAddr: l.Addr().String(), LogLevel: "error", LogFormat: "text"}
	logger, lvl := newLogger(cfg, io.Discard)
	start := time.Now()
	err = run(context.Background(), cfg, logger, lvl)
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("err = %v, want a 'not available' error", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("took %s, should fail immediately", time.Since(start))
	}
}

func TestCheckGrpcAddress(t *testing.T) {
	if err := checkGrpcAddress(freePort(t)); err != nil {
		t.Fatalf("free port rejected: %v", err)
	}
	if err := checkGrpcAddress("unix:///tmp/does-not-matter.sock"); err != nil {
		t.Fatalf("unix address should be skipped: %v", err)
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	if err := checkGrpcAddress(freePort(t) + "," + l.Addr().String()); err == nil {
		t.Fatal("a taken address in a list must be reported")
	}
}
