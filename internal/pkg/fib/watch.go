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

package fib

import (
	"context"
	"time"

	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/server"
)

// Watch subscribes the syncer to best path changes of a running BGP server,
// starting with the current best paths. The callback runs on the watcher's own
// goroutine, one event at a time, so the syncer is the only writer of the
// kernel table and the BGP loop never waits for netlink. Events queue without
// bound while a callback is slow. The subscription ends when ctx is cancelled.
func (s *Syncer) Watch(ctx context.Context, bgp *server.BgpServer) error {
	return bgp.WatchEvent(ctx, server.WatchEventMessageCallbacks{
		OnBestPath: func(paths []*apiutil.Path, _ time.Time) { s.HandleBestPaths(paths) },
	}, server.WatchBestPath(true))
}
