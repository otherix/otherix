// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package blobbroker

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// RetentionStore is the storage surface the saga retention sweep needs.
// *etcdstore.Store satisfies it.
type RetentionStore interface {
	DeleteExpiredPullSagas(ctx context.Context, cutoff time.Time) (int, error)
}

// SagaRetentionFunc returns the periodic function that deletes terminal blob-pull
// saga records older than the retention window, bounding the saga key space (each
// blob pull writes one record). Mirrors the task cleanup sweep.
func SagaRetentionFunc(st RetentionStore, retention time.Duration, log *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		deleted, err := st.DeleteExpiredPullSagas(ctx, time.Now().Add(-retention))
		if err != nil {
			return fmt.Errorf("delete expired pull sagas: %v", err)
		}
		if deleted > 0 {
			log.InfoContext(ctx, "artifact.saga.retention", slog.Int("deleted", deleted))
		}
		return nil
	}
}
