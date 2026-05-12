// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package tasks

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"github.com/otherix/otherix/internal/store"
)

// Default retention windows - the tasks resource has retention
// semantics independent of river_job. These are the working defaults;
// future per-task-type overrides would land alongside the type-keyed
// SQL change.
const (
	defaultCompletedRetention = 7 * 24 * time.Hour
	defaultFailedRetention    = 30 * 24 * time.Hour

	// cleanupInterval is the cadence at which the cleanup job
	// fires. Hourly is far below both retention windows, so deletion
	// lag is well under a percent of the windows themselves.
	cleanupInterval = 1 * time.Hour
)

// CleanupArgs is the JobArgs for the periodic deletion of finalized
// tasks past their per-state retention window. Empty payload — every
// fact the worker needs is computed from the configured retention
// plus time.Now().
type CleanupArgs struct{}

// Kind returns the job kind. Domain-prefix `tasks.` matches the
// owning sub-package; the verb mirrors the SQL query name.
func (CleanupArgs) Kind() string { return "tasks.cleanup" }

// cleaner is the package-private narrow interface the cleanup worker
// depends on. *store.Queries satisfies it implicitly through its
// sqlc-generated DeleteExpiredTasks; the narrowing keeps the worker
// testable against a hand-rolled fake without dragging the entire
// Queries surface into a mock.
type cleaner interface {
	DeleteExpiredTasks(ctx context.Context, arg store.DeleteExpiredTasksParams) (int64, error)
}

// RetentionConfig holds the per-state retention durations the cleanup
// worker keys deletion off. Zero values fall back to the package
// defaults at registration time via withDefaults; the field exists as
// a seam so tests can pin compressed durations and cmd/api/main.go
// can wire APIConfig overrides through the same Deps shape.
type RetentionConfig struct {
	Completed time.Duration
	Failed    time.Duration
}

// withDefaults returns a copy of c with any zero-valued field
// replaced by the package default. Non-zero fields pass through
// unchanged.
func (c RetentionConfig) withDefaults() RetentionConfig {
	out := c
	if out.Completed <= 0 {
		out.Completed = defaultCompletedRetention
	}
	if out.Failed <= 0 {
		out.Failed = defaultFailedRetention
	}
	return out
}

// Deps is the tasks-domain dependency bundle for river-job
// composition. Callers (typically cmd/api/main.go via
// internal/api.BuildRiverClient) pass *store.Store; Workers narrows
// it to the package-private cleaner interface at registration time.
type Deps struct {
	Store     *store.Store
	Retention RetentionConfig
	Logger    *slog.Logger
}

// CleanupWorker deletes finalized tasks past their per-state
// retention window. Driven by river's periodic scheduler (see
// PeriodicJobs); the per-fire payload is empty, so the worker
// computes its own cutoffs from time.Now at run time.
type CleanupWorker struct {
	river.WorkerDefaults[CleanupArgs]
	cleaner   cleaner
	retention RetentionConfig
	logger    *slog.Logger
}

// Work runs one cleanup pass. Logs the deleted count at INFO so
// cluster operators can watch the schedule fire even when there is
// no work to do (a quiet "deleted=0" line is the liveness signal).
func (w *CleanupWorker) Work(ctx context.Context, _ *river.Job[CleanupArgs]) error {
	now := time.Now()
	deleted, err := w.cleaner.DeleteExpiredTasks(ctx, store.DeleteExpiredTasksParams{
		CompletedCutoff: now.Add(-w.retention.Completed),
		FailedCutoff:    now.Add(-w.retention.Failed),
	})
	if err != nil {
		return fmt.Errorf("delete expired tasks: %v", err)
	}
	w.logger.InfoContext(ctx, "tasks.cleanup", "deleted", deleted)
	return nil
}

// Workers registers every tasks-domain worker on the supplied
// bundle. Currently ships exactly one (cleanup); future tasks-domain
// workers add AddWorker calls here without changing call sites in
// cmd/api/main.go.
func Workers(workers *river.Workers, deps Deps) {
	river.AddWorker(workers, &CleanupWorker{
		cleaner:   deps.Store.Queries(),
		retention: deps.Retention.withDefaults(),
		logger:    deps.Logger,
	})
}

// PeriodicJobs returns every periodic schedule the tasks domain
// wants the river client to run. RunOnStart is false so api-server
// deploys do not trigger a sweep on every restart.
func PeriodicJobs(_ Deps) []*river.PeriodicJob {
	return []*river.PeriodicJob{
		river.NewPeriodicJob(
			river.PeriodicInterval(cleanupInterval),
			func() (river.JobArgs, *river.InsertOpts) {
				return CleanupArgs{}, nil
			},
			&river.PeriodicJobOpts{RunOnStart: false},
		),
	}
}
