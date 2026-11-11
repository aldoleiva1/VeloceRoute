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
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"strings"
)

// config is the whole configuration of the skeleton: just enough to start the
// BGP engine and its gRPC API. A real config file schema is task E2-2.
type config struct {
	ASN        uint32
	RouterID   string
	ListenPort int32  // BGP listen port; -1 disables the listener
	GrpcAddr   string // gRPC API address; localhost by default (task E8-1)
	LogLevel   string
	LogFormat  string
	Version    bool
}

func parseFlags(args []string, errOut io.Writer) (*config, error) {
	c := &config{}
	fs := flag.NewFlagSet("veloced", flag.ContinueOnError)
	fs.SetOutput(errOut)
	asn := fs.Uint("asn", 0, "local AS number (required)")
	fs.StringVar(&c.RouterID, "router-id", "", "BGP router ID, an IPv4 address (required)")
	listen := fs.Int("listen-port", 179, "TCP port for incoming BGP sessions; -1 disables the listener")
	fs.StringVar(&c.GrpcAddr, "grpc-address", "127.0.0.1:50051", "gRPC API listen address (host:port or unix://path)")
	fs.StringVar(&c.LogLevel, "log-level", "info", "log level: debug, info, warn, error")
	fs.StringVar(&c.LogFormat, "log-format", "text", "log format: text or json")
	fs.BoolVar(&c.Version, "version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if *asn > 0xFFFFFFFF {
		return nil, fmt.Errorf("-asn %d does not fit in 32 bits", *asn)
	}
	c.ASN = uint32(*asn)
	c.ListenPort = int32(*listen)
	if c.Version {
		return c, nil
	}
	return c, c.validate()
}

func (c *config) validate() error {
	var errs []error
	if c.ASN == 0 {
		errs = append(errs, errors.New("-asn is required and must be non-zero"))
	}
	if addr, err := netip.ParseAddr(c.RouterID); err != nil || !addr.Is4() {
		errs = append(errs, fmt.Errorf("-router-id %q must be an IPv4 address", c.RouterID))
	}
	if c.ListenPort < -1 || c.ListenPort > 65535 {
		errs = append(errs, fmt.Errorf("-listen-port %d is out of range (-1 to 65535)", c.ListenPort))
	}
	if c.GrpcAddr == "" {
		errs = append(errs, errors.New("-grpc-address must not be empty"))
	}
	if _, err := parseLevel(c.LogLevel); err != nil {
		errs = append(errs, err)
	}
	if c.LogFormat != "text" && c.LogFormat != "json" {
		errs = append(errs, fmt.Errorf("-log-format %q must be text or json", c.LogFormat))
	}
	return errors.Join(errs...)
}

func parseLevel(s string) (slog.Level, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return 0, fmt.Errorf("-log-level %q must be debug, info, warn or error", s)
	}
	return l, nil
}

// newLogger builds the structured logger and the level variable the BGP
// server uses, so the level can be changed at run time.
func newLogger(c *config, out io.Writer) (*slog.Logger, *slog.LevelVar) {
	lvl := new(slog.LevelVar)
	l, _ := parseLevel(c.LogLevel)
	lvl.Set(l)
	opts := &slog.HandlerOptions{Level: lvl}
	if c.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(out, opts)), lvl
	}
	return slog.New(slog.NewTextHandler(out, opts)), lvl
}

func stderr() io.Writer { return os.Stderr }
