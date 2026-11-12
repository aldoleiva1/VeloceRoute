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
	"log/slog"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// Go garbage collector defaults (task E2-5). The numbers come from the 900k
// route experiments in docs/veloceroute/performance.md: GOGC=200 takes about
// a fifth off the CPU of a full-table import for a quarter more memory, and a
// memory limit keeps a large table from pushing the host into swap or the OOM
// killer.
const (
	// gcPercentDefault is used when the machine has enough memory.
	gcPercentDefault = 200
	// gcPercentSmall is the Go default, kept on small machines where the
	// extra memory would matter.
	gcPercentSmall = 100
	// smallMemory is the size below which gcPercentSmall is used.
	smallMemory = 2 << 30
	// memoryLimitPercent is the share of available memory the soft limit
	// allows the Go heap to grow to.
	memoryLimitPercent = 75
)

// gcSettings is what applyGC decided; a zero field means "left alone".
type gcSettings struct {
	Percent     int    // GOGC value set by us (0: not set by us)
	PercentFrom string // "default", "small-memory" or "env"
	Limit       int64  // soft memory limit in bytes set by us (0: not set by us)
	LimitFrom   string // "cgroup", "host", "env" or "none"
	Available   uint64 // memory the limit was derived from (0: unknown)
}

// chooseGC decides the settings. env reports whether GOGC / GOMEMLIMIT are
// set in the environment (they always win; the Go runtime has already applied
// them). avail is the memory available to the process, 0 if unknown, and
// source says where it came from.
func chooseGC(env func(string) bool, avail uint64, source string) gcSettings {
	var s gcSettings
	s.Available = avail
	switch {
	case env("GOGC"):
		s.PercentFrom = "env"
	case avail != 0 && avail < smallMemory:
		s.Percent, s.PercentFrom = gcPercentSmall, "small-memory"
	default:
		s.Percent, s.PercentFrom = gcPercentDefault, "default"
	}
	switch {
	case env("GOMEMLIMIT"):
		s.LimitFrom = "env"
	case avail == 0:
		s.LimitFrom = "none"
	default:
		s.Limit = int64(avail * memoryLimitPercent / 100)
		s.LimitFrom = source
	}
	return s
}

// applyGC sets the collector parameters at start-up and logs what it chose.
func applyGC(logger *slog.Logger) gcSettings {
	avail, source := availableMemory(os.ReadFile)
	s := chooseGC(func(k string) bool { _, ok := os.LookupEnv(k); return ok }, avail, source)
	if s.Percent != 0 {
		debug.SetGCPercent(s.Percent)
	}
	if s.Limit != 0 {
		debug.SetMemoryLimit(s.Limit)
	}
	logger.Info("go gc settings",
		slog.Int("gogc", s.Percent), slog.String("gogc_from", s.PercentFrom),
		slog.Int64("memory_limit_bytes", s.Limit), slog.String("memory_limit_from", s.LimitFrom),
		slog.Uint64("available_bytes", s.Available))
	return s
}

// availableMemory is the memory this process may use: the container limit if
// there is one below the host's memory, else the host's. It returns 0 if
// neither can be read (not Linux). read is os.ReadFile, replaceable in tests.
func availableMemory(read func(string) ([]byte, error)) (uint64, string) {
	host := hostMemory(read)
	cg := cgroupLimit(read)
	switch {
	case cg != 0 && (host == 0 || cg < host):
		return cg, "cgroup"
	case host != 0:
		return host, "host"
	}
	return 0, "none"
}

// cgroupLimit reads the memory limit of the container, cgroup v2 first. "max"
// or an absurdly large number (v1 reports page-rounded max int64) mean none.
func cgroupLimit(read func(string) ([]byte, error)) uint64 {
	for _, p := range []string{
		"/sys/fs/cgroup/memory.max",
		"/sys/fs/cgroup/memory/memory.limit_in_bytes",
	} {
		b, err := read(p)
		if err != nil {
			continue
		}
		v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
		if err != nil || v == 0 || v >= 1<<60 {
			continue
		}
		return v
	}
	return 0
}

// hostMemory reads MemTotal from /proc/meminfo, in bytes.
func hostMemory(read func(string) ([]byte, error)) uint64 {
	b, err := read("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "MemTotal:" {
			kb, err := strconv.ParseUint(f[1], 10, 64)
			if err != nil {
				return 0
			}
			return kb << 10
		}
	}
	return 0
}
