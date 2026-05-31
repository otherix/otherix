// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package middleware

import (
	"context"
	"fmt"
	"log/slog"
)

// IdempotencyCleaner is the storage surface the idempotency-key retention sweep
// depends on. Both *store.Store and *etcdstore.Store satisfy it.
type IdempotencyCleaner interface {
	DeleteExpiredIdempotencyKeys(ctx context.Context) (int64, error)
}

// IdempotencyCleanupFunc returns the periodic function that deletes
// idempotency-key rows past their TTL. The etcd-runtime replacement for the
// river IdempotencyCleanupWorker; the worker.Scheduler drives it.
func IdempotencyCleanupFunc(st IdempotencyCleaner, log *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		deleted, err := st.DeleteExpiredIdempotencyKeys(ctx)
		if err != nil {
			return fmt.Errorf("delete expired idempotency keys: %v", err)
		}
		log.InfoContext(ctx, "idempotency.cleanup", "deleted", deleted)
		return nil
	}
}
