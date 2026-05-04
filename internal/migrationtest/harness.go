// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package migrationtest spins up an ephemeral Postgres 16 container and applies
// the project's pressly/goose migrations against it via internal/store/migrate.
// Used by integration tests in tests/migrations and internal/store.
package migrationtest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/otherix/otherix/internal/store/migrate"
)

// Harness owns one Postgres container and a pgx pool against it. One Harness
// per test (or per test package) is appropriate; share via TestMain when many
// tests in a package need it.
type Harness struct {
	Pool      *pgxpool.Pool
	DSN       string
	container *tcpostgres.PostgresContainer
}

// Start launches a Postgres 16 container, applies all migrations via
// migrate.Run(ActionUp), and returns a Harness. Callers must call Stop.
func Start(ctx context.Context) (*Harness, error) {
	h, err := newContainer(ctx)
	if err != nil {
		return nil, err
	}
	if err := migrate.Run(ctx, h.Pool, migrate.ActionUp, nil); err != nil {
		h.Stop(ctx)
		return nil, fmt.Errorf("apply migrations: %w", err)
	}
	return h, nil
}

// NewIsolated starts a fresh Postgres container exactly like Start but does
// NOT apply migrations. Useful for tests that exercise the migration machinery
// itself (apply, rollback, re-apply). Caller owns the lifecycle and must call
// Stop.
func NewIsolated(ctx context.Context) (*Harness, error) {
	return newContainer(ctx)
}

// Stop closes the pool and terminates the container. Safe to call once.
func (h *Harness) Stop(ctx context.Context) {
	if h.Pool != nil {
		h.Pool.Close()
	}
	if h.container != nil {
		_ = h.container.Terminate(ctx)
	}
}

// MustStart is a test-helper variant that fails the test if Start fails.
// Cleanup is registered via t.Cleanup so callers don't have to defer Stop.
func MustStart(t *testing.T) *Harness {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	h, err := Start(ctx)
	if err != nil {
		t.Fatalf("migrationtest.Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		h.Stop(stopCtx)
	})
	return h
}

func newContainer(ctx context.Context) (*Harness, error) {
	c, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("otherix_test"),
		tcpostgres.WithUsername("otherix"),
		tcpostgres.WithPassword("otherix"),
		// Two readiness signals layered with wait.ForAll: the postgres
		// log line confirms the server is up *inside* the container, and
		// ForListeningPort confirms Docker has published the port on the
		// host. Log-only readiness was racy on busy daemons — Inspect
		// could surface the container before NetworkSettings.Ports were
		// populated, and the subsequent ConnectionString call failed with
		// `port "5432/tcp" not found`.
		testcontainers.WithWaitStrategy(
			wait.ForAll(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2),
				wait.ForListeningPort("5432/tcp"),
			).WithStartupTimeoutDefault(60*time.Second),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("start postgres: %w", err)
	}

	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, fmt.Errorf("get dsn: %w", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, fmt.Errorf("pgx pool: %w", err)
	}

	return &Harness{Pool: pool, DSN: dsn, container: c}, nil
}
