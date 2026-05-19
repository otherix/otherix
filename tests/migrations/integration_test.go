// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/migrationtest"
	"github.com/otherix/otherix/internal/store/migrate"
)

func TestSchemaApplies(t *testing.T) {
	h := shared
	var n int
	err := h.Pool.QueryRow(context.Background(),
		`select count(*) from goose_db_version where version_id = 1 and is_applied = true`,
	).Scan(&n)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected goose_db_version to record version 1 as applied, got %d", n)
	}
}

func TestDownThenUpIsIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	h, err := migrationtest.NewIsolated(ctx)
	if err != nil {
		t.Fatalf("NewIsolated: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		h.Stop(stopCtx)
	})

	if err := migrate.Run(ctx, h.Pool, migrate.ActionUp, nil); err != nil {
		t.Fatalf("migrate.Run(ActionUp) #1: %v", err)
	}
	if err := migrate.Run(ctx, h.Pool, migrate.ActionDown, nil); err != nil {
		t.Fatalf("migrate.Run(ActionDown): %v", err)
	}
	if err := migrate.Run(ctx, h.Pool, migrate.ActionUp, nil); err != nil {
		t.Fatalf("migrate.Run(ActionUp) #2: %v", err)
	}

	var n int
	err = h.Pool.QueryRow(ctx,
		`select count(*) from information_schema.tables where table_name = 'vms'`,
	).Scan(&n)
	if err != nil {
		t.Fatalf("verify vms table: %v", err)
	}
	if n != 1 {
		t.Fatalf("vms table missing after down+up, count=%d", n)
	}
}
