// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"testing"
)

// TestRiverMigrationMarkersV1ToV6 verifies the precondition that
// makes 00002_river_v6.go's Up function a no-op against the current
// schema: 00001_init.sql seeds river_migration with rows for every
// upstream version 1..6 the embedded schema covers. If this
// invariant breaks (e.g. someone edits 00001 to drop the seed
// INSERT), the seam in 00002 stops being a no-op and starts failing
// with table-already-exists conflicts; the assertion below makes
// that failure mode loud rather than silent.
func TestRiverMigrationMarkersV1ToV6(t *testing.T) {
	ctx := context.Background()
	rows, err := shared.Pool.Query(ctx,
		`select version from river_migration where line = 'main' order by version`)
	if err != nil {
		t.Fatalf("query river_migration: %v", err)
	}
	defer rows.Close()

	var versions []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan version: %v", err)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows iter: %v", err)
	}

	want := []int{1, 2, 3, 4, 5, 6}
	if len(versions) < len(want) {
		t.Fatalf("river_migration markers = %v, want at least %v", versions, want)
	}
	for i, w := range want {
		if versions[i] != w {
			t.Fatalf("river_migration[%d] = %d, want %d (full set: %v)", i, versions[i], w, versions)
		}
	}
}
