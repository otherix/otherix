// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package migrations

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/otherix/otherix/internal/store"
)

// Default retention windows for the migration retention sweep. Only TERMINAL
// migrations are ever deleted; in-flight migrations are never swept. Mirror the
// tasks resource convention: completed 7d, failed / cancelled 30d.
const (
	defaultCompletedRetention = 7 * 24 * time.Hour
	defaultFailedRetention    = 30 * 24 * time.Hour
	defaultCancelledRetention = 30 * 24 * time.Hour
)

// RetentionConfig holds the per-state retention durations the cleanup function
// keys deletion off. Zero values fall back to the package defaults via
// withDefaults; the field exists as a seam so tests can pin compressed durations
// and cmd/api can wire APIConfig overrides through the same shape. Mirrors
// tasks.RetentionConfig.
type RetentionConfig struct {
	Completed time.Duration
	Failed    time.Duration
	Cancelled time.Duration
}

// withDefaults returns a copy of c with any zero-valued field replaced by the
// package default. Non-zero fields pass through unchanged.
func (c RetentionConfig) withDefaults() RetentionConfig {
	out := c
	if out.Completed <= 0 {
		out.Completed = defaultCompletedRetention
	}
	if out.Failed <= 0 {
		out.Failed = defaultFailedRetention
	}
	if out.Cancelled <= 0 {
		out.Cancelled = defaultCancelledRetention
	}
	return out
}

// CleanupStore is the storage surface the migration retention sweep depends on.
// *etcdstore.Store satisfies it.
type CleanupStore interface {
	DeleteExpiredMigrations(ctx context.Context, arg store.DeleteExpiredMigrationsParams) (int64, error)
}

// CleanupFunc returns the periodic function that deletes TERMINAL migrations past
// their per-state retention window, so `migration list` does not grow forever. In
// flight migrations (pending/setup/active/postcopy_active) are never swept - the
// store enforces the terminal-only invariant. The Scheduler drives it. Mirrors
// tasks.CleanupFunc.
func CleanupFunc(st CleanupStore, retention RetentionConfig, log *slog.Logger) func(context.Context) error {
	rc := retention.withDefaults()
	return func(ctx context.Context) error {
		now := time.Now()
		deleted, err := st.DeleteExpiredMigrations(ctx, store.DeleteExpiredMigrationsParams{
			CompletedCutoff: now.Add(-rc.Completed),
			FailedCutoff:    now.Add(-rc.Failed),
			CancelledCutoff: now.Add(-rc.Cancelled),
		})
		if err != nil {
			return fmt.Errorf("delete expired migrations: %v", err)
		}
		log.InfoContext(ctx, "migrations.cleanup", "deleted", deleted)
		return nil
	}
}
