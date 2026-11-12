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

// veloced is the VeloceRoute daemon: one static binary that embeds the GoBGP
// control plane and its gRPC API. WireGuard tunnels, Linux kernel route sync
// and Amazon VPC Route Server peering are added as lifecycle stages (see
// ROADMAP.md, tasks E2 to E5).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/osrg/gobgp/v4/internal/pkg/version"
)

func main() {
	cfg, err := parseFlags(os.Args[1:], stderr())
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(stderr(), "veloced:", err)
		os.Exit(2)
	}
	if cfg.Version {
		fmt.Println("veloced", version.Version())
		return
	}
	logger, lvl := newLogger(cfg, stderr())
	applyGC(logger)

	// SIGINT and SIGTERM stop the daemon. SIGHUP is reserved for config reload
	// (task E2-3) and is only logged for now.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			logger.Warn("SIGHUP received: config reload is not implemented yet")
		}
	}()

	if err := run(ctx, cfg, logger, lvl); err != nil {
		logger.Error("veloced failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
}
