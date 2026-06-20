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
	DeleteStrandedPullSagas(ctx context.Context, cutoff time.Time) (int, error)
}

// SagaRetentionFunc returns the periodic function that bounds the saga key space
// (each blob pull writes one record). It reaps terminal sagas older than the
// retention window and, with a separate generous cutoff, stranded non-terminal
// sagas a control-plane crash left behind so they do not leak forever. Mirrors
// the task cleanup sweep.
func SagaRetentionFunc(st RetentionStore, retention, strandedRetention time.Duration, log *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		now := time.Now()
		deleted, err := st.DeleteExpiredPullSagas(ctx, now.Add(-retention))
		if err != nil {
			return fmt.Errorf("delete expired pull sagas: %v", err)
		}
		stranded, err := st.DeleteStrandedPullSagas(ctx, now.Add(-strandedRetention))
		if err != nil {
			return fmt.Errorf("delete stranded pull sagas: %v", err)
		}
		if deleted > 0 || stranded > 0 {
			log.InfoContext(ctx, "artifact.saga.retention",
				slog.Int("deleted", deleted), slog.Int("stranded", stranded))
		}
		return nil
	}
}
