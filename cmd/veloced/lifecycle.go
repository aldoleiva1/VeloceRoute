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
	"errors"
	"fmt"
	"log/slog"
)

// stage is one part of the daemon with an ordered start and stop. Later
// features add their own stages (WireGuard tunnels, kernel route sync) so that
// the daemon starts them in dependency order and stops them in reverse.
type stage struct {
	name  string
	start func(ctx context.Context) error
	stop  func(ctx context.Context) error
}

// lifecycle starts stages in order and stops them in reverse order.
type lifecycle struct {
	logger  *slog.Logger
	stages  []stage
	started []stage
}

// startAll starts every stage in order. If one fails, the stages that already
// started are stopped in reverse order and the start error is returned.
func (l *lifecycle) startAll(ctx context.Context) error {
	for _, s := range l.stages {
		l.logger.Info("starting", slog.String("stage", s.name))
		if err := s.start(ctx); err != nil {
			err = fmt.Errorf("start %s: %w", s.name, err)
			if stopErr := l.stopAll(context.WithoutCancel(ctx)); stopErr != nil {
				err = errors.Join(err, stopErr)
			}
			return err
		}
		l.started = append(l.started, s)
	}
	return nil
}

// stopAll stops the started stages in reverse order. It keeps going after a
// failure so that every stage gets its chance to clean up, and returns all
// errors together.
func (l *lifecycle) stopAll(ctx context.Context) error {
	var errs []error
	for i := len(l.started) - 1; i >= 0; i-- {
		s := l.started[i]
		l.logger.Info("stopping", slog.String("stage", s.name))
		if s.stop == nil {
			continue
		}
		if err := s.stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop %s: %w", s.name, err))
		}
	}
	l.started = nil
	return errors.Join(errs...)
}
