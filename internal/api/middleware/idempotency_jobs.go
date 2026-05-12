// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package middleware

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"github.com/otherix/otherix/internal/store"
)

// IdempotencyCleanupArgs is the JobArgs for the periodic deletion of
// idempotency_keys rows past their expires_at. Empty payload — the
// SQL query computes its own cutoff via `expires_at < now()`, so the
// worker has nothing to feed in.
type IdempotencyCleanupArgs struct{}

// Kind returns the job kind. Domain-prefix `idempotency.` matches the
// owning subsystem; the verb mirrors the SQL query name.
func (IdempotencyCleanupArgs) Kind() string { return "idempotency.cleanup" }

// idempotencyCleaner is the package-private narrow interface the
// cleanup worker depends on. Named with the package-prefix to avoid
// collision with the existing IdempotencyStore interface (which is
// the middleware's runtime surface against the same store, but a
// different subset of methods). *store.Queries satisfies it
// implicitly through its sqlc-generated method set.
type idempotencyCleaner interface {
	DeleteExpiredIdempotencyKeys(ctx context.Context) (int64, error)
}

// IdempotencyDeps is the dependency bundle for idempotency-domain
// river-job composition. Callers (typically cmd/api/main.go via
// internal/api.BuildRiverClient) pass *store.Store; IdempotencyWorkers
// narrows it to the package-private idempotencyCleaner interface at
// registration time.
type IdempotencyDeps struct {
	Store  *store.Store
	Logger *slog.Logger
}

// IdempotencyCleanupWorker deletes idempotency_keys rows whose
// expires_at is in the past. The SQL query has no state filter, so
// both `in_flight` and `completed` rows past TTL are removed —
// abandoned in-flight rows do not survive past their TTL just because
// the original handler crashed.
type IdempotencyCleanupWorker struct {
	river.WorkerDefaults[IdempotencyCleanupArgs]
	cleaner idempotencyCleaner
	logger  *slog.Logger
}

// Work runs one cleanup pass. Logs the deleted count at INFO so a
// quiet "deleted=0" line still serves as the schedule's liveness
// signal.
func (w *IdempotencyCleanupWorker) Work(ctx context.Context, _ *river.Job[IdempotencyCleanupArgs]) error {
	deleted, err := w.cleaner.DeleteExpiredIdempotencyKeys(ctx)
	if err != nil {
		return fmt.Errorf("delete expired idempotency keys: %v", err)
	}
	w.logger.InfoContext(ctx, "idempotency.cleanup", "deleted", deleted)
	return nil
}

// IdempotencyWorkers registers idempotency-domain workers on the
// supplied bundle. Currently ships exactly one (cleanup); future
// idempotency-related jobs add AddWorker calls here without changing
// call sites in cmd/api/main.go.
func IdempotencyWorkers(workers *river.Workers, deps IdempotencyDeps) {
	river.AddWorker(workers, &IdempotencyCleanupWorker{
		cleaner: deps.Store.Queries(),
		logger:  deps.Logger,
	})
}

// idempotencyCleanupInterval is the cadence at which the cleanup job
// fires. Hourly keeps idempotency_keys table size bounded by mutation
// rate × ~25 hours (24h TTL set by the middleware on insertion plus
// at most one polling interval of lag).
const idempotencyCleanupInterval = 1 * time.Hour

// IdempotencyPeriodicJobs returns every periodic schedule the
// idempotency subsystem wants the river client to run. RunOnStart is
// false so api-server deploys do not trigger a sweep on every restart.
func IdempotencyPeriodicJobs(_ IdempotencyDeps) []*river.PeriodicJob {
	return []*river.PeriodicJob{
		river.NewPeriodicJob(
			river.PeriodicInterval(idempotencyCleanupInterval),
			func() (river.JobArgs, *river.InsertOpts) {
				return IdempotencyCleanupArgs{}, nil
			},
			&river.PeriodicJobOpts{RunOnStart: false},
		),
	}
}
