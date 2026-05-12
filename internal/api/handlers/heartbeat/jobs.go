// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"github.com/otherix/otherix/internal/store"
)

const (
	defaultStaleThreshold = 90 * time.Second
	defaultInterval       = 30 * time.Second
)

// ReconcileArgs is the JobArgs for the periodic node-health
// reconciliation. Empty payload — every fact the worker needs is
// computed from the configured StaleThreshold plus time.Now().
type ReconcileArgs struct{}

// Kind returns the job kind. Domain prefix `heartbeat.` matches the
// owning sub-package; the verb names the operation.
func (ReconcileArgs) Kind() string { return "heartbeat.reconcile" }

// reconciler is the package-private narrow interface the worker
// depends on. *store.Queries satisfies it implicitly through the two
// sqlc-generated methods; the narrowing keeps the worker testable
// against a hand-rolled fake without dragging the entire Queries
// surface in.
type reconciler interface {
	PromoteHealthyNodes(ctx context.Context, freshAfter time.Time) ([]store.PromoteHealthyNodesRow, error)
	MarkNodesUnreachable(ctx context.Context, staleBefore time.Time) ([]store.MarkNodesUnreachableRow, error)
}

// ReconcileConfig pins the timing knobs the worker keys off. Zero
// values fall back to package defaults at registration time via
// withDefaults; the field exists as a seam so tests can pin
// compressed durations and cmd/api/main.go can wire APIConfig
// overrides through the same Deps shape.
type ReconcileConfig struct {
	StaleThreshold time.Duration
	Interval       time.Duration
}

func (c ReconcileConfig) withDefaults() ReconcileConfig {
	out := c
	if out.StaleThreshold <= 0 {
		out.StaleThreshold = defaultStaleThreshold
	}
	if out.Interval <= 0 {
		out.Interval = defaultInterval
	}
	return out
}

// ReconcileDeps is the dependency bundle for the reconciler worker.
// Logger is required; Store is narrowed to the package-private
// reconciler interface at registration time.
type ReconcileDeps struct {
	Store  *store.Store
	Config ReconcileConfig
	Logger *slog.Logger
}

// ReconcileWorker flips nodes between 'ready' and 'unreachable' on a
// fixed cadence. Two passes per tick:
//   - PromoteHealthyNodes converts 'pending' / 'unreachable' rows
//     with a fresh heartbeat к 'ready';
//   - MarkNodesUnreachable demotes 'ready' / 'pending' rows whose
//     heartbeat has lapsed.
//
// 'cordoned' and 'draining' are operator-pinned and untouched.
type ReconcileWorker struct {
	river.WorkerDefaults[ReconcileArgs]
	reconciler reconciler
	cfg        ReconcileConfig
	logger     *slog.Logger
}

// Work runs one reconciliation pass.
func (w *ReconcileWorker) Work(ctx context.Context, _ *river.Job[ReconcileArgs]) error {
	now := time.Now()
	freshAfter := now.Add(-w.cfg.StaleThreshold)

	promoted, err := w.reconciler.PromoteHealthyNodes(ctx, freshAfter)
	if err != nil {
		return fmt.Errorf("promote healthy nodes: %v", err)
	}
	for _, row := range promoted {
		w.logger.InfoContext(ctx, "node promoted to ready",
			slog.String("node_id", row.ID.String()),
			slog.String("node_name", row.Name),
		)
	}

	demoted, err := w.reconciler.MarkNodesUnreachable(ctx, freshAfter)
	if err != nil {
		return fmt.Errorf("mark nodes unreachable: %v", err)
	}
	for _, row := range demoted {
		var lastHeartbeat string
		if row.LastHeartbeatAt != nil {
			lastHeartbeat = row.LastHeartbeatAt.Format(time.RFC3339Nano)
		} else {
			lastHeartbeat = "never"
		}
		w.logger.WarnContext(ctx, "node marked unreachable",
			slog.String("node_id", row.ID.String()),
			slog.String("node_name", row.Name),
			slog.String("last_heartbeat_at", lastHeartbeat),
		)
	}

	if len(promoted) == 0 && len(demoted) == 0 {
		w.logger.DebugContext(ctx, "heartbeat.reconcile no-op")
	}
	return nil
}

// Workers registers the heartbeat-domain workers on the supplied
// bundle.
func Workers(workers *river.Workers, deps ReconcileDeps) {
	river.AddWorker(workers, &ReconcileWorker{
		reconciler: deps.Store.Queries(),
		cfg:        deps.Config.withDefaults(),
		logger:     deps.Logger,
	})
}

// PeriodicJobs returns the periodic schedule for the reconciler.
// RunOnStart is true so an api-server restart promotes any node that
// has continued heartbeating during the outage без waiting for the
// next tick.
func PeriodicJobs(deps ReconcileDeps) []*river.PeriodicJob {
	cfg := deps.Config.withDefaults()
	return []*river.PeriodicJob{
		river.NewPeriodicJob(
			river.PeriodicInterval(cfg.Interval),
			func() (river.JobArgs, *river.InsertOpts) {
				return ReconcileArgs{}, nil
			},
			&river.PeriodicJobOpts{RunOnStart: true},
		),
	}
}
