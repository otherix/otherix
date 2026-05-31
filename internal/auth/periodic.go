// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// RefreshTokenCleaner is the storage surface the refresh-token retention sweep
// depends on. *etcdstore.Store satisfies it.
type RefreshTokenCleaner interface {
	DeleteExpiredRefreshTokens(ctx context.Context, cutoff time.Time) (int64, error)
}

// RefreshTokenCleanupFunc returns the periodic function that deletes refresh
// tokens past their expiry. The etcd-runtime replacement for the river
// RefreshTokenCleanupWorker; the worker.Scheduler drives it.
func RefreshTokenCleanupFunc(st RefreshTokenCleaner, log *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		deleted, err := st.DeleteExpiredRefreshTokens(ctx, time.Now())
		if err != nil {
			return fmt.Errorf("delete expired refresh tokens: %v", err)
		}
		log.InfoContext(ctx, "auth.refresh_token_cleanup", "deleted", deleted)
		return nil
	}
}
