// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"github.com/otherix/otherix/internal/store"
)

// RefreshTokenCleanupArgs is the JobArgs for the periodic deletion of
// refresh-token rows past their expires_at. The args carry no payload —
// every fact the worker needs is computed at run time.
type RefreshTokenCleanupArgs struct{}

// Kind returns the job kind. Domain-prefix `auth.` matches the owning
// package; the verb mirrors the SQL query name.
func (RefreshTokenCleanupArgs) Kind() string { return "auth.refresh_token_cleanup" }

// cleaner is the package-private narrow interface the cleanup worker
// depends on. *store.Queries satisfies it implicitly through its
// sqlc-generated methods, which keeps the worker testable without
// dragging the entire Queries surface into a mock.
type cleaner interface {
	DeleteExpiredRefreshTokens(ctx context.Context, cutoff time.Time) (int64, error)
}

// Deps is the auth-domain dependency bundle for river-job composition.
// Callers (typically cmd/api/main.go via internal/api.BuildRiverClient)
// pass *store.Store; Workers narrows it to the package-private cleaner
// interface at registration time.
type Deps struct {
	Store  *store.Store
	Logger *slog.Logger
}

// RefreshTokenCleanupWorker deletes refresh_token rows whose expires_at
// is in the past. Driven by river's periodic scheduler (see PeriodicJobs);
// the per-fire payload is empty, so the worker computes its own cutoff
// from time.Now at run time.
type RefreshTokenCleanupWorker struct {
	river.WorkerDefaults[RefreshTokenCleanupArgs]
	cleaner cleaner
	logger  *slog.Logger
}

// Work runs one cleanup pass. Logs the deleted count at INFO so cluster
// operators can watch the schedule fire even when there is no work to
// do (a quiet "deleted=0" line is itself the liveness signal).
func (w *RefreshTokenCleanupWorker) Work(ctx context.Context, _ *river.Job[RefreshTokenCleanupArgs]) error {
	deleted, err := w.cleaner.DeleteExpiredRefreshTokens(ctx, time.Now())
	if err != nil {
		return fmt.Errorf("delete expired refresh tokens: %v", err)
	}
	w.logger.InfoContext(ctx, "auth.refresh_token_cleanup", "deleted", deleted)
	return nil
}

// Workers registers every auth-domain worker on the supplied bundle.
// Currently ships exactly one (refresh-token cleanup); future workers add
// AddWorker calls here without changing call sites in cmd/api/main.go.
func Workers(workers *river.Workers, deps Deps) {
	river.AddWorker(workers, &RefreshTokenCleanupWorker{
		cleaner: deps.Store.Queries(),
		logger:  deps.Logger,
	})
}

// refreshTokenCleanupInterval is the cadence at which the cleanup job
// fires. Hourly is comfortably below the 30-day refresh-token TTL, so
// up-to-one-hour deletion lag is invisible to clients and well under
// any realistic auditing budget.
const refreshTokenCleanupInterval = 1 * time.Hour

// PeriodicJobs returns every periodic schedule the auth domain wants
// the river client to run. RunOnStart is false so api-server deploys
// do not trigger a sweep on every restart.
func PeriodicJobs(_ Deps) []*river.PeriodicJob {
	return []*river.PeriodicJob{
		river.NewPeriodicJob(
			river.PeriodicInterval(refreshTokenCleanupInterval),
			func() (river.JobArgs, *river.InsertOpts) {
				return RefreshTokenCleanupArgs{}, nil
			},
			&river.PeriodicJobOpts{RunOnStart: false},
		),
	}
}
