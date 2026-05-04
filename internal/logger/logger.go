// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package logger configures slog handlers used by every Otherix binary.
//
// All log keys are snake_case to match the API JSON convention. Use
// WithComponent to tag each binary's logger with its component name
// (api / scheduler / reconciler / agent).
package logger

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// Config selects the log level and output format. Unknown values fall back
// to "info" and "json" respectively rather than failing — startup logging
// must never block process start.
type Config struct {
	Level  string `koanf:"level"`
	Format string `koanf:"format"`
}

// New returns a slog.Logger configured per cfg, writing to stdout.
func New(cfg Config) *slog.Logger {
	return newWithWriter(cfg, os.Stdout)
}

func newWithWriter(cfg Config, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(cfg.Level)}

	var h slog.Handler
	switch strings.ToLower(cfg.Format) {
	case "text":
		h = slog.NewTextHandler(w, opts)
	default:
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(h)
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// WithComponent returns a logger that tags every record with the given
// component name under the "component" key. Used by each binary's main
// to mark log output with "api", "scheduler", "reconciler", or "agent".
func WithComponent(l *slog.Logger, component string) *slog.Logger {
	return l.With("component", component)
}

// SetDefault installs l as the slog default logger so packages that use the
// package-level slog functions (slog.Info, slog.Error, ...) inherit our
// formatting and level.
func SetDefault(l *slog.Logger) {
	slog.SetDefault(l)
}
