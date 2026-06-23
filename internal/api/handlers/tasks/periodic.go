// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package tasks

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/otherix/otherix/internal/store"
)

// CleanupStore is the storage surface the retention sweep depends on.
// *etcdstore.Store satisfies it.
type CleanupStore interface {
	DeleteExpiredTasks(ctx context.Context, arg store.DeleteExpiredTasksParams) (int64, error)
}

// CleanupFunc returns the periodic function that deletes finalized tasks past
// their per-state retention window. The etcd-runtime retention sweep; the
// Scheduler drives it.
func CleanupFunc(st CleanupStore, retention RetentionConfig, log *slog.Logger) func(context.Context) error {
	rc := retention.withDefaults()
	return func(ctx context.Context) error {
		now := time.Now()
		deleted, err := st.DeleteExpiredTasks(ctx, store.DeleteExpiredTasksParams{
			CompletedCutoff: now.Add(-rc.Completed),
			FailedCutoff:    now.Add(-rc.Failed),
		})
		if err != nil {
			return fmt.Errorf("delete expired tasks: %v", err)
		}
		log.InfoContext(ctx, "tasks.cleanup", "deleted", deleted)
		return nil
	}
}
