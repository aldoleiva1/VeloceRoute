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
	"io"
	"log/slog"
	"os"
	"runtime/debug"
	"testing"
)

func fakeFiles(files map[string]string) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		if s, ok := files[p]; ok {
			return []byte(s), nil
		}
		return nil, errors.New("no such file")
	}
}

const gib = 1 << 30

func TestAvailableMemory(t *testing.T) {
	meminfo := "MemTotal:       16777216 kB\nMemFree: 1 kB\n" // 16 GiB
	cases := []struct {
		name   string
		files  map[string]string
		want   uint64
		source string
	}{
		{"host only", map[string]string{"/proc/meminfo": meminfo}, 16 * gib, "host"},
		{"cgroup v2 below host", map[string]string{"/proc/meminfo": meminfo, "/sys/fs/cgroup/memory.max": "4294967296\n"}, 4 * gib, "cgroup"},
		{"cgroup v2 unlimited", map[string]string{"/proc/meminfo": meminfo, "/sys/fs/cgroup/memory.max": "max\n"}, 16 * gib, "host"},
		{"cgroup v1", map[string]string{"/proc/meminfo": meminfo, "/sys/fs/cgroup/memory/memory.limit_in_bytes": "2147483648"}, 2 * gib, "cgroup"},
		{"cgroup v1 unlimited", map[string]string{"/proc/meminfo": meminfo, "/sys/fs/cgroup/memory/memory.limit_in_bytes": "9223372036854771712"}, 16 * gib, "host"},
		{"cgroup above host", map[string]string{"/proc/meminfo": meminfo, "/sys/fs/cgroup/memory.max": "68719476736"}, 16 * gib, "host"},
		{"cgroup without meminfo", map[string]string{"/sys/fs/cgroup/memory.max": "1073741824"}, 1 * gib, "cgroup"},
		{"nothing readable", nil, 0, "none"},
		{"garbage", map[string]string{"/proc/meminfo": "MemTotal: lots kB", "/sys/fs/cgroup/memory.max": "x"}, 0, "none"},
	}
	for _, c := range cases {
		got, src := availableMemory(fakeFiles(c.files))
		if got != c.want || src != c.source {
			t.Errorf("%s: got %d from %s, want %d from %s", c.name, got, src, c.want, c.source)
		}
	}
}

func TestChooseGC(t *testing.T) {
	none := func(string) bool { return false }
	only := func(k string) func(string) bool { return func(x string) bool { return x == k } }
	cases := []struct {
		name      string
		env       func(string) bool
		avail     uint64
		wantPct   int
		wantFrom  string
		wantLimit int64
		limFrom   string
	}{
		{"big box", none, 16 * gib, 200, "default", 12 * gib, "cgroup"},
		{"limit is 75%", none, 4 * gib, 200, "default", 3 * gib, "cgroup"},
		{"small box keeps 100", none, 1 * gib, 100, "small-memory", 768 << 20, "cgroup"},
		{"exactly 2 GiB is not small", none, 2 * gib, 200, "default", 1536 << 20, "cgroup"},
		{"unknown memory", none, 0, 200, "default", 0, "none"},
		{"GOGC wins", only("GOGC"), 16 * gib, 0, "env", 12 * gib, "cgroup"},
		{"GOMEMLIMIT wins", only("GOMEMLIMIT"), 16 * gib, 200, "default", 0, "env"},
	}
	for _, c := range cases {
		s := chooseGC(c.env, c.avail, "cgroup")
		if s.Percent != c.wantPct || s.PercentFrom != c.wantFrom || s.Limit != c.wantLimit || s.LimitFrom != c.limFrom {
			t.Errorf("%s: got %+v", c.name, s)
		}
	}
}

// applyGC changes process-wide settings, so the test restores them.
func TestApplyGCSetsTheRuntime(t *testing.T) {
	oldPct := debug.SetGCPercent(100)
	oldLimit := debug.SetMemoryLimit(-1)
	defer func() { debug.SetGCPercent(oldPct); debug.SetMemoryLimit(oldLimit) }()
	t.Setenv("GOGC", "")
	t.Setenv("GOMEMLIMIT", "")
	// Unset (not just empty) so the "environment wins" path is not taken.
	unsetEnv(t, "GOGC")
	unsetEnv(t, "GOMEMLIMIT")

	s := applyGC(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if s.Percent != 0 {
		if got := debug.SetGCPercent(100); got != s.Percent {
			t.Errorf("runtime GOGC %d, want %d", got, s.Percent)
		}
	}
	if s.Limit != 0 {
		if got := debug.SetMemoryLimit(-1); got != s.Limit {
			t.Errorf("runtime memory limit %d, want %d", got, s.Limit)
		}
	}
}

func TestEnvironmentLeavesTheRuntimeAlone(t *testing.T) {
	oldPct := debug.SetGCPercent(150)
	oldLimit := debug.SetMemoryLimit(5 * gib)
	defer func() { debug.SetGCPercent(oldPct); debug.SetMemoryLimit(oldLimit) }()
	t.Setenv("GOGC", "150")
	t.Setenv("GOMEMLIMIT", "5GiB")
	s := applyGC(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if s.Percent != 0 || s.Limit != 0 {
		t.Fatalf("set %+v although the environment has both", s)
	}
	if got := debug.SetGCPercent(150); got != 150 {
		t.Errorf("GOGC changed to %d", got)
	}
	if got := debug.SetMemoryLimit(-1); got != 5*gib {
		t.Errorf("limit changed to %d", got)
	}
}

func unsetEnv(t *testing.T, k string) {
	t.Helper()
	t.Setenv(k, "x") // registers the restore of the original value
	if err := os.Unsetenv(k); err != nil {
		t.Fatal(err)
	}
}
