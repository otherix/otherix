// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package middleware

import (
	"context"
	"errors"
	"log/slog"
)

// IdempotencyCleaner is the storage surface the idempotency retention sweeps
// depend on: the idempotency-key rows and the separate idempotency-task index.
// *etcdstore.Store satisfies it.
type IdempotencyCleaner interface {
	DeleteExpiredIdempotencyKeys(ctx context.Context) (int64, error)
	DeleteExpiredIdempotencyTaskIndex(ctx context.Context) (int64, error)
}

// IdempotencyCleanupFunc returns the periodic function that reaps both the
// idempotency-key rows and the idempotency-task index past their TTLs. The two
// are independent sweeps (the index carries the full 24h horizon, the rows a
// shorter in_flight lease), so a failure of one must not skip the other: both
// run, both counts are logged, and the errors are joined. The worker.Scheduler
// drives it.
func IdempotencyCleanupFunc(st IdempotencyCleaner, log *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		keys, keysErr := st.DeleteExpiredIdempotencyKeys(ctx)
		index, indexErr := st.DeleteExpiredIdempotencyTaskIndex(ctx)
		log.InfoContext(ctx, "idempotency.cleanup", "deleted_keys", keys, "deleted_index", index)
		return errors.Join(keysErr, indexErr)
	}
}
