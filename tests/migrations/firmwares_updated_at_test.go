// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"testing"
	"time"
)

// TestFirmwareUpdatedAtPresent confirms migration 00005 added the
// `updated_at` column with a NOT NULL constraint and the
// trg_set_updated_at trigger keeps it current on UPDATE.
func TestFirmwareUpdatedAtPresent(t *testing.T) {
	h := shared
	ctx := context.Background()

	var col string
	var notNullable bool
	err := h.Pool.QueryRow(ctx, `
		select column_name, is_nullable = 'NO'
		from information_schema.columns
		where table_name = 'firmwares' and column_name = 'updated_at'`).Scan(&col, &notNullable)
	if err != nil {
		t.Fatalf("query updated_at column: %v", err)
	}
	if !notNullable {
		t.Fatalf("firmwares.updated_at must be NOT NULL")
	}

	var fwID string
	if err := h.Pool.QueryRow(ctx, `
		insert into firmwares (name, architecture, type)
		values ('OVMF-arm64-updated-at', 'arm64', 'uefi')
		returning id`).Scan(&fwID); err != nil {
		t.Fatalf("insert firmware: %v", err)
	}

	var created, updated time.Time
	if err := h.Pool.QueryRow(ctx,
		`select created_at, updated_at from firmwares where id = $1`, fwID).
		Scan(&created, &updated); err != nil {
		t.Fatalf("read times: %v", err)
	}
	if updated.Before(created) {
		t.Fatalf("updated_at (%v) must be >= created_at (%v) on insert", updated, created)
	}

	time.Sleep(2 * time.Millisecond)

	if _, err := h.Pool.Exec(ctx,
		`update firmwares set version = '4.0' where id = $1`, fwID); err != nil {
		t.Fatalf("update firmware: %v", err)
	}

	var bumped time.Time
	if err := h.Pool.QueryRow(ctx,
		`select updated_at from firmwares where id = $1`, fwID).Scan(&bumped); err != nil {
		t.Fatalf("read bumped: %v", err)
	}
	if !bumped.After(updated) {
		t.Fatalf("updated_at trigger did not advance the column: before=%v after=%v", updated, bumped)
	}
}
