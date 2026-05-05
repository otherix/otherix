// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package migrations holds Go-style goose migrations registered via
// init(). SQL migrations in this directory are discovered through
// migrate.go's go:embed FS instead.
//
// File 00007_river_v6.go is the goose Go-bridge seam for river queue
// migrations.
//
// Behavioural note: 00001_init.sql already embeds the river v0.35.1
// schema verbatim and seeds river_migration with rows for versions
// 1..6. This seam therefore reports zero pending versions on Up; it
// exists as the on-ramp for future river bumps (next bump adds
// 00NNN_river_vN.go with an updated TargetVersion) and as the
// canonical place where the rivermigrate API is wired into goose.
//
// Down is a no-op rather than a literal mirror of Up: river-table
// state belongs to 00001's verbatim-embed, so reversing the seam
// must not drop those tables. Future bump-files (which advance
// schema version by version) will have non-trivial Down behaviour.
package migrations

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/pressly/goose/v3"
	"github.com/riverqueue/river/riverdriver/riverdatabasesql"
	"github.com/riverqueue/river/rivermigrate"
)

// riverTargetVersion pins the river schema version this seam targets.
// Bumping the river module requires a new 00NNN_river_vN.go file
// with an updated constant; this file is never edited in place.
const riverTargetVersion = 6

func init() {
	goose.AddMigrationNoTxContext(upRiverV6, downRiverV6)
}

func upRiverV6(ctx context.Context, db *sql.DB) error {
	// Probe river_migration for the current applied version. When the
	// schema is already at or beyond the seam's target — the R5-a case
	// where 00001's verbatim embed seeded river_migration with rows
	// 1..6 — rivermigrate.Migrate refuses to run with the strict
	// "version N is not in target list of valid migrations to apply"
	// error. Skipping the call preserves the no-op-against-current
	// schema property the seam exists to express.
	var current int
	if err := db.QueryRowContext(ctx,
		`select coalesce(max(version), 0) from river_migration where line = 'main'`,
	).Scan(&current); err != nil {
		return fmt.Errorf("probe river_migration: %v", err)
	}
	if current >= riverTargetVersion {
		return nil
	}

	m, err := rivermigrate.New(riverdatabasesql.New(db), nil)
	if err != nil {
		return fmt.Errorf("rivermigrate new: %v", err)
	}
	if _, err := m.Migrate(ctx, rivermigrate.DirectionUp, &rivermigrate.MigrateOpts{
		TargetVersion: riverTargetVersion,
	}); err != nil {
		return fmt.Errorf("river migrate up: %v", err)
	}
	return nil
}

func downRiverV6(_ context.Context, _ *sql.DB) error {
	// No-op: river tables were created by 00001_init.sql's verbatim
	// embed of upstream river schema. This seam adds nothing to the
	// schema, so reversing it removes nothing. Reverting the river
	// schema itself is what 00001's Down covers (drop public schema).
	return nil
}
