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

//go:build !linux

package fib

import (
	"net/netip"
)

// Defaults match the Linux values so that flag defaults are the same on every
// platform.
const (
	DefaultProtocol = 201
	DefaultMetric   = 20
	DefaultTable    = 254
)

// KernelOptions is only used on Linux.
type KernelOptions struct {
	Table    int
	Protocol int
	Metric   int
}

type unsupported struct{}

func (unsupported) Replace(Route) error         { return ErrKernelUnsupported }
func (unsupported) Delete(netip.Prefix) error   { return ErrKernelUnsupported }
func (unsupported) ListOwned() ([]Route, error) { return nil, ErrKernelUnsupported }
func (unsupported) Close() error                { return nil }

func NewKernelProgrammer(KernelOptions) (interface {
	Programmer
	Close() error
}, error) {
	return nil, ErrKernelUnsupported
}

// CheckPrivileges always fails: there is no kernel route programmer here.
func CheckPrivileges() error { return ErrKernelUnsupported }
