// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"context"
	"fmt"
	"log/slog"
	mathrand "math/rand/v2"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/otherix/otherix/internal/store"
)

const (
	defaultScanTriggerInterval = 15 * time.Minute
	defaultScanTriggerJitter   = 30 * time.Second
)

// ScanTriggerArgs is the JobArgs for the periodic storage_pool.scan
// trigger. The body is empty — the trigger queries fresh pool state
// at fire time, so encoding it into args would only invite drift.
type ScanTriggerArgs struct{}

// Kind names the job kind. Distinct from `storage_pool.scan` (the
// per-pool unit of work) so river surfaces the two cleanly in
// observability tooling и so the periodic dedup window stays scoped
// to the trigger itself.
func (ScanTriggerArgs) Kind() string { return "storage_pool.scan_trigger" }

// ScanTriggerConfig pins the timing knobs the periodic trigger keys
// off. Zero values fall back to package defaults via withDefaults at
// registration time, mirroring the heartbeat reconciler pattern.
type ScanTriggerConfig struct {
	// Interval between trigger firings. Defaults to 15 minutes —
	// pool capacity changes far slower than CPU / memory (which use
	// 30 s heartbeat), и filesystem walks dominate scan cost.
	Interval time.Duration

	// Jitter caps the random delay applied к each per-pool enqueue
	// within a trigger fire. Spreads agent-side load when many pools
	// would otherwise scan back-to-back. Must be strictly less than
	// Interval at validation time. Defaults to 30 s.
	Jitter time.Duration
}

// withDefaults backfills package defaults for zero-valued knobs. Mirrors
// the heartbeat reconciler pattern: time.Duration cannot distinguish
// "unset" от an explicit zero, so we treat <= 0 as "use the default."
// Operators who legitimately want zero jitter (single-pool clusters,
// deterministic test runs) bypass the config seam и invoke jitterDelay
// directly — а non-issue at MVP scale.
func (c ScanTriggerConfig) withDefaults() ScanTriggerConfig {
	out := c
	if out.Interval <= 0 {
		out.Interval = defaultScanTriggerInterval
	}
	if out.Jitter <= 0 {
		out.Jitter = defaultScanTriggerJitter
	}
	return out
}

// ScanTriggerDeps is the dependency bundle for the periodic trigger.
// RiverClient is intentionally absent — workers extract it from the
// Work() context at fire time via river.ClientFromContext so the
// registration order (workers first, client second) closes cleanly.
type ScanTriggerDeps struct {
	Store  *store.Store
	Config ScanTriggerConfig
	Logger *slog.Logger
}

// triggerStore narrows *store.Store к the surface the trigger needs.
// Lets the unit tests pass а stub без the entire Queries struct.
type triggerStore interface {
	ListPoolsNeedingScan(ctx context.Context) ([]store.ListPoolsNeedingScanRow, error)
}

// triggerEnqueuer abstracts the per-pool atomic enqueue (CreateTask +
// InsertTx + UpdateTaskRiverJobID). Production wires
// productionTriggerEnqueuer over *store.Store + *river.Client; tests
// supply а recording fake.
type triggerEnqueuer interface {
	EnqueueScan(ctx context.Context, poolID uuid.UUID, scheduledAt time.Time) error
}

// ScanTriggerWorker fans out per-pool scan tasks on a configured cadence.
// Each fire:
//
//  1. Reads ListPoolsNeedingScan — pools on scannable nodes без an
//     in-flight scan task (handler-side dedup, see the SQL comment).
//  2. For each pool, schedules а fresh scan task с а jittered
//     ScheduledAt so concurrent agent walks spread.
//  3. Errors on individual pools log а WARN и do not block siblings;
//     the periodic schedule recovers automatically on the next fire.
type ScanTriggerWorker struct {
	river.WorkerDefaults[ScanTriggerArgs]
	store  *store.Store
	jitter time.Duration
	logger *slog.Logger
}

// Work runs one trigger pass. The river client is pulled from context
// (river.ClientFromContext) so the worker can be registered before
// BuildRiverClient returns the client it produces — closing the
// otherwise-circular construction order.
func (w *ScanTriggerWorker) Work(ctx context.Context, _ *river.Job[ScanTriggerArgs]) error {
	client := river.ClientFromContext[pgx.Tx](ctx)
	enq := &productionTriggerEnqueuer{store: w.store, client: client}
	return runScanTriggerTick(ctx, w.store.Queries(), enq, w.jitter, w.logger)
}

// runScanTriggerTick is the testable seam. It iterates pools-needing-
// scan и delegates per-pool enqueue к the supplied enqueuer. Per-pool
// errors are logged и swallowed; the only fatal error is а failure
// to list pools (which prevents the whole tick).
func runScanTriggerTick(
	ctx context.Context,
	src triggerStore,
	enq triggerEnqueuer,
	jitter time.Duration,
	logger *slog.Logger,
) error {
	pools, err := src.ListPoolsNeedingScan(ctx)
	if err != nil {
		return fmt.Errorf("list pools needing scan: %v", err)
	}

	now := time.Now()
	var enqueued, failed int
	for _, p := range pools {
		scheduledAt := now.Add(jitterDelay(jitter))
		if err := enq.EnqueueScan(ctx, p.ID, scheduledAt); err != nil {
			logger.WarnContext(ctx, "storage_pool.scan_trigger enqueue failed",
				slog.String("pool_id", p.ID.String()),
				slog.String("pool_name", p.Name),
				slog.Any("error", err),
			)
			failed++
			continue
		}
		enqueued++
	}

	logger.InfoContext(ctx, "storage_pool.scan_trigger tick",
		slog.Int("pools_eligible", len(pools)),
		slog.Int("enqueued", enqueued),
		slog.Int("failed", failed),
	)
	return nil
}

// jitterDelay returns а random delay in [0, max). Zero or negative
// max disables jitter (deterministic, immediate enqueue) — useful in
// tests и whenever an operator pins Jitter=0 в config.
//
// math/rand/v2 is deliberate here: the jitter spreads agent-side
// load — а scheduling concern, not а security primitive — и keying
// off the global PRNG is exactly the kubernetes-scheduler practice.
func jitterDelay(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(mathrand.Int64N(int64(max))) //nolint:gosec // G404: jitter is а scheduling primitive, not security-sensitive
}

// productionTriggerEnqueuer is the real-world implementation: opens
// а transaction, inserts the `tasks` row + the river job atomically,
// и stamps the river_job_id back onto the task row. Mirrors the
// operator-triggered enqueueScan path в scan.go exactly (CreatedBy is
// nil because the periodic trigger has no user identity).
type productionTriggerEnqueuer struct {
	store  *store.Store
	client *river.Client[pgx.Tx]
}

func (e *productionTriggerEnqueuer) EnqueueScan(ctx context.Context, poolID uuid.UUID, scheduledAt time.Time) error {
	taskID := uuid.New()
	pid := poolID
	return e.store.InTxWithTx(ctx, func(q *store.Queries, tx pgx.Tx) error {
		if _, err := q.CreateTask(ctx, store.CreateTaskParams{
			ID:           taskID,
			Type:         "storage_pool.scan",
			Status:       store.TaskStatusPending,
			ResourceType: "storage_pool",
			ResourceID:   &pid,
			Args:         []byte(`{}`),
			MaxAttempts:  25,
			CreatedBy:    nil,
		}); err != nil {
			return fmt.Errorf("create task: %v", err)
		}

		insertResult, err := e.client.InsertTx(ctx, tx,
			StoragePoolScanArgs{TaskID: taskID, PoolID: poolID},
			&river.InsertOpts{ScheduledAt: scheduledAt},
		)
		if err != nil {
			return fmt.Errorf("river insert tx: %v", err)
		}

		jobID := insertResult.Job.ID
		if err := q.UpdateTaskRiverJobID(ctx, store.UpdateTaskRiverJobIDParams{
			ID:         taskID,
			RiverJobID: &jobID,
		}); err != nil {
			return fmt.Errorf("update task river_job_id: %v", err)
		}
		return nil
	})
}

// ScanTriggerWorkers registers the periodic-trigger worker on the
// supplied bundle. Registration is unconditional once enabled — the
// periodic schedule is gated separately в ScanTriggerPeriodicJobs.
func ScanTriggerWorkers(workers *river.Workers, deps ScanTriggerDeps) {
	cfg := deps.Config.withDefaults()
	river.AddWorker(workers, &ScanTriggerWorker{
		store:  deps.Store,
		jitter: cfg.Jitter,
		logger: deps.Logger,
	})
}

// ScanTriggerPeriodicJobs returns the periodic schedule for the
// trigger. RunOnStart is false: an api-server restart should not
// race с in-flight scan tasks от the previous run, и the operator-
// triggered `POST /v1/storage-pools/{id}/scan` path stays the
// on-demand escape hatch для immediate scans.
func ScanTriggerPeriodicJobs(deps ScanTriggerDeps) []*river.PeriodicJob {
	cfg := deps.Config.withDefaults()
	return []*river.PeriodicJob{
		river.NewPeriodicJob(
			river.PeriodicInterval(cfg.Interval),
			func() (river.JobArgs, *river.InsertOpts) {
				return ScanTriggerArgs{}, nil
			},
			&river.PeriodicJobOpts{RunOnStart: false},
		),
	}
}
