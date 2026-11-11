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
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	api "github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/internal/pkg/version"
	"github.com/osrg/gobgp/v4/pkg/server"
)

// stopTimeout bounds how long shutdown may take before run gives up waiting.
const stopTimeout = 30 * time.Second

// checkGrpcAddress fails if a TCP address of the gRPC API cannot be bound.
// GoBGP itself only logs a failed bind and carries on without an API, which
// would leave a daemon that looks healthy but cannot be managed. Probing first
// lets veloced refuse to start. It is a pre-check: another process could still
// take the port between the probe and GoBGP's own bind. Unix sockets are not
// probed because that would mean removing a possibly live socket file.
func checkGrpcAddress(addr string) error {
	for _, host := range strings.Split(addr, ",") {
		if strings.HasPrefix(host, "unix://") {
			continue
		}
		l, err := net.Listen("tcp", host)
		if err != nil {
			return fmt.Errorf("gRPC address %q is not available: %w", host, err)
		}
		l.Close()
	}
	return nil
}

// bgpStage runs the embedded GoBGP engine and its gRPC API.
func bgpStage(cfg *config, logger *slog.Logger, lvl *slog.LevelVar) stage {
	var bgp *server.BgpServer
	return stage{
		name: "bgp",
		start: func(ctx context.Context) error {
			if err := checkGrpcAddress(cfg.GrpcAddr); err != nil {
				return err
			}
			bgp = server.NewBgpServer(
				server.GrpcListenAddress(cfg.GrpcAddr),
				server.LoggerOption(logger, lvl),
			)
			go bgp.Serve()
			return bgp.StartBgp(ctx, &api.StartBgpRequest{
				Global: &api.Global{
					Asn:        cfg.ASN,
					RouterId:   cfg.RouterID,
					ListenPort: cfg.ListenPort,
				},
			})
		},
		stop: func(ctx context.Context) error {
			if bgp == nil {
				return nil
			}
			// Stop calls StopBgp first (withdrawing our routes and closing
			// sessions cleanly), then stops BFD and the gRPC API.
			bgp.Stop()
			return nil
		},
	}
}

// run starts the daemon and blocks until ctx is cancelled (a signal, or a
// test), then shuts down in reverse start order. It returns the start error,
// or the shutdown errors if shutdown failed.
func run(ctx context.Context, cfg *config, logger *slog.Logger, lvl *slog.LevelVar) error {
	lc := &lifecycle{
		logger: logger,
		stages: []stage{bgpStage(cfg, logger, lvl)},
	}
	return runStages(ctx, lc, func() {
		logger.Info("veloced started",
			slog.String("version", version.Version()),
			slog.Uint64("asn", uint64(cfg.ASN)),
			slog.String("router_id", cfg.RouterID),
			slog.String("grpc", cfg.GrpcAddr))
	})
}

func runStages(ctx context.Context, lc *lifecycle, started func()) error {
	if err := lc.startAll(ctx); err != nil {
		return err
	}
	started()
	<-ctx.Done()
	lc.logger.Info("shutting down", slog.String("reason", context.Cause(ctx).Error()))
	// The parent context is already cancelled; shutdown needs its own deadline.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
	defer cancel()
	return lc.stopAll(sctx)
}
