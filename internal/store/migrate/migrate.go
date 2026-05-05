// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package migrate runs the project's pressly/goose migrations against a
// PostgreSQL pool. The migrations are embedded into the binary via go:embed
// so the api-server can apply them without external files.
package migrate

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	// Side-effect import: registers Go-style migrations (e.g. river
	// schema seam in 00007_river_v6.go) via goose.AddMigration*
	// calls in the package's init().
	_ "github.com/otherix/otherix/internal/store/migrate/migrations"
)

//go:embed migrations/*.sql
var embeddedMigrations embed.FS

const migrationsDir = "migrations"

// Action is one of the three operations Run accepts.
type Action string

// The supported actions.
const (
	ActionUp     Action = "up"
	ActionDown   Action = "down"
	ActionStatus Action = "status"
)

// Set goose's dialect once at package load. SetDialect mutates a goose
// package-level variable and is not safe to call concurrently. Doing it
// in init keeps the call site predictable.
func init() {
	if err := goose.SetDialect("postgres"); err != nil {
		panic(fmt.Sprintf("goose.SetDialect: %v", err))
	}
	goose.SetBaseFS(embeddedMigrations)
}

// Run executes action against the schema reachable through pool. The pool
// is borrowed, not consumed; the caller still owns it and is responsible
// for closing it. Diagnostic output from goose is written to log at info
// level; goose.Logger.Fatalf is mapped to log.Error and an explicit error.
//
// Run is not safe for concurrent invocation: goose.SetLogger mutates a
// package-level variable in goose, so two Runs racing would interleave
// loggers. In practice migrations are one-shot — this is a contract note,
// not a defended invariant.
func Run(ctx context.Context, pool *pgxpool.Pool, action Action, log *slog.Logger) error {
	if pool == nil {
		return errors.New("pool is nil")
	}
	if log == nil {
		log = slog.Default()
	}

	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }()

	goose.SetLogger(slogGooseLogger{log: log})

	switch action {
	case ActionUp:
		return runWithErr("up", goose.UpContext(ctx, db, migrationsDir))
	case ActionDown:
		return runWithErr("down", goose.DownContext(ctx, db, migrationsDir))
	case ActionStatus:
		return runWithErr("status", goose.StatusContext(ctx, db, migrationsDir))
	default:
		return fmt.Errorf("unknown action %q (want up, down, or status)", action)
	}
}

func runWithErr(name string, err error) error {
	if err != nil {
		return fmt.Errorf("goose %s: %v", name, err)
	}
	return nil
}

// slogGooseLogger adapts slog to goose's Logger interface. goose only calls
// Printf for diagnostics and Fatalf for fatal errors; we surface the latter
// via slog at error level (the actual termination is the caller's choice
// based on the error returned from Run).
type slogGooseLogger struct{ log *slog.Logger }

func (l slogGooseLogger) Fatalf(format string, v ...any) {
	l.log.Error(fmt.Sprintf(format, v...))
}

func (l slogGooseLogger) Printf(format string, v ...any) {
	l.log.Info(fmt.Sprintf(format, v...))
}

// compile-time assertion that the type satisfies goose.Logger.
var _ goose.Logger = (*slogGooseLogger)(nil)
