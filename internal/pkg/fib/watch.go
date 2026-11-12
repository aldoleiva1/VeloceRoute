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

	"github.com/osrg/gobgp/v4/pkg/server"
)

// Watch subscribes the syncer to best path changes of a running BGP server,
// starting with the current best paths. The callback only queues a compact
// copy of the event, so the BGP watcher never waits for the kernel; a writer
// goroutine applies whatever has queued up as one batch (the busier the
// kernel, the bigger the batch), so the syncer is the only writer of the kernel
// table. The queue is unbounded: a slow kernel means memory grows, not that
// BGP slows down. Both end when ctx is cancelled.
func (s *Syncer) Watch(ctx context.Context, bgp *server.BgpServer) error {
	go s.drain(ctx)
	return bgp.WatchEvent(ctx, server.WatchEventMessageCallbacks{
		OnBestPathChange: func(changes []server.BestPathChange, _ time.Time) { s.SubmitChanges(changes) },
	}, server.WatchBestPathChanges(true))
}
