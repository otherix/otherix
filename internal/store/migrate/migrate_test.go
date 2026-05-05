// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package migrate_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/migrationtest"
	"github.com/otherix/otherix/internal/store/migrate"
)

func TestRunUpStatusDownUp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	h, err := migrationtest.NewIsolated(ctx)
	if err != nil {
		t.Fatalf("migrationtest.NewIsolated: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		h.Stop(stopCtx)
	})

	log := slog.Default()

	if err := migrate.Run(ctx, h.Pool, migrate.ActionUp, log); err != nil {
		t.Fatalf("Run(ActionUp) initial = %v, want nil", err)
	}

	var n int
	if err := h.Pool.QueryRow(ctx,
		`select count(*) from information_schema.tables where table_schema='public' and table_name='users'`,
	).Scan(&n); err != nil {
		t.Fatalf("query users existence: %v", err)
	}
	if n != 1 {
		t.Errorf("after ActionUp: users table count = %d, want 1", n)
	}

	if err := migrate.Run(ctx, h.Pool, migrate.ActionStatus, log); err != nil {
		t.Fatalf("Run(ActionStatus) = %v, want nil", err)
	}

	if err := migrate.Run(ctx, h.Pool, migrate.ActionDown, log); err != nil {
		t.Fatalf("Run(ActionDown) = %v, want nil", err)
	}

	if err := migrate.Run(ctx, h.Pool, migrate.ActionUp, log); err != nil {
		t.Fatalf("Run(ActionUp) after Down = %v, want nil", err)
	}

	if err := migrate.Run(ctx, h.Pool, "sideways", log); err == nil {
		t.Errorf(`Run(_, _, "sideways", _) = nil, want error`)
	}
}

func TestRunNilPool(t *testing.T) {
	if err := migrate.Run(context.Background(), nil, migrate.ActionUp, slog.Default()); err == nil {
		t.Errorf("Run(nil pool) = nil, want error")
	}
}
