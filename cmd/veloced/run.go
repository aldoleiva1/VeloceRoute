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
	"github.com/osrg/gobgp/v4/internal/pkg/fib"
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

// daemon is what the stages share. The bgp stage fills in the server; later
// stages (kernel sync, and WireGuard before it) use it.
type daemon struct {
	bgp *server.BgpServer
}

// bgpStage runs the embedded GoBGP engine and its gRPC API.
func bgpStage(cfg *config, logger *slog.Logger, lvl *slog.LevelVar, d *daemon) stage {
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
			d.bgp = bgp
			go bgp.Serve()
			return bgp.StartBgp(ctx, &api.StartBgpRequest{
				Global: &api.Global{
					Asn:              cfg.ASN,
					RouterId:         cfg.RouterID,
					ListenPort:       cfg.ListenPort,
					UseMultiplePaths: cfg.Multipath,
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

// kernelRoutes is the part of the fib package veloced needs, so tests can
// replace the Linux-only implementation with a fake.
type kernelRoutes struct {
	check func() error
	open  func(fib.KernelOptions) (routeProgrammer, error)
}

type routeProgrammer interface {
	fib.Programmer
	Close() error
}

func realKernel() kernelRoutes {
	return kernelRoutes{
		check: fib.CheckPrivileges,
		open: func(o fib.KernelOptions) (routeProgrammer, error) {
			return fib.NewKernelProgrammer(o)
		},
	}
}

// fibStage copies BGP best paths into the kernel routing table. It starts after
// bgp, so there is a server to watch, and stops before it: it removes the
// routes it installed while the daemon is still shutting down in order.
func fibStage(cfg *config, logger *slog.Logger, d *daemon, k kernelRoutes) stage {
	var (
		syn    *fib.Syncer
		prog   routeProgrammer
		cancel context.CancelFunc
	)
	return stage{
		name: "fib",
		start: func(ctx context.Context) error {
			if err := k.check(); err != nil {
				return err
			}
			var err error
			prog, err = k.open(fib.KernelOptions{Table: cfg.FibTable, Protocol: cfg.FibProtocol, Metric: cfg.FibMetric})
			if err != nil {
				return err
			}
			// Events carry every equal-cost path only when BGP keeps them, so
			// the syncer must be told which kind of event stream it gets.
			var opts []fib.Option
			if cfg.Multipath {
				opts = append(opts, fib.WithMultipath(cfg.FibMaxPaths))
			}
			syn = fib.NewSyncer(prog, logger, opts...)
			// No session is up yet, so any route of ours already in the kernel
			// is left over from an earlier run and is not valid.
			if err := syn.Reconcile(); err != nil {
				prog.Close()
				return err
			}
			// The watch has its own lifetime so that stop, not the cancelled
			// run context, decides when it ends.
			var wctx context.Context
			wctx, cancel = context.WithCancel(context.WithoutCancel(ctx))
			// Watch the network before the BGP events, so nothing that happens
			// in between is missed: routes the kernel drops are restored and
			// routes waiting for a next hop are retried.
			if src, ok := prog.(fib.ChangeSource); ok {
				if err := syn.Run(wctx, src, cfg.FibRetry); err != nil {
					cancel()
					prog.Close()
					return err
				}
			}
			if err := syn.Watch(wctx, d.bgp); err != nil {
				cancel()
				prog.Close()
				return err
			}
			logger.Info("kernel route sync on",
				slog.Int("protocol", cfg.FibProtocol), slog.Int("table", cfg.FibTable), slog.Int("metric", cfg.FibMetric),
				slog.Bool("ecmp", cfg.Multipath), slog.Int("max_paths", cfg.FibMaxPaths))
			return nil
		},
		stop: func(context.Context) error {
			if cancel != nil {
				cancel()
			}
			var err error
			if syn != nil {
				err = syn.RemoveAll()
			}
			if prog != nil {
				prog.Close()
			}
			return err
		},
	}
}

// run starts the daemon and blocks until ctx is cancelled (a signal, or a
// test), then shuts down in reverse start order. It returns the start error,
// or the shutdown errors if shutdown failed.
func run(ctx context.Context, cfg *config, logger *slog.Logger, lvl *slog.LevelVar) error {
	return runWithKernel(ctx, cfg, logger, lvl, realKernel())
}

func runWithKernel(ctx context.Context, cfg *config, logger *slog.Logger, lvl *slog.LevelVar, k kernelRoutes) error {
	d := &daemon{}
	stages := []stage{bgpStage(cfg, logger, lvl, d)}
	if cfg.FibSync {
		stages = append(stages, fibStage(cfg, logger, d, k))
	}
	lc := &lifecycle{logger: logger, stages: stages}
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
