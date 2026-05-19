// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"testing"

	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

func TestRiverHasNoPendingMigrations(t *testing.T) {
	ctx := context.Background()
	driver := riverpgxv5.New(shared.Pool)
	migrator, err := rivermigrate.New(driver, nil)
	if err != nil {
		t.Fatalf("rivermigrate.New: %v", err)
	}
	res, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, &rivermigrate.MigrateOpts{
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("migrator.Migrate dry-run: %v", err)
	}
	if len(res.Versions) != 0 {
		var versions []int
		for _, v := range res.Versions {
			versions = append(versions, v.Version)
		}
		t.Fatalf("river reports %d migrations are pending; embedded schema is out of sync. Pending versions: %v", len(res.Versions), versions)
	}
}
