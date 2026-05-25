// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/otherix/otherix/internal/agentapi"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/store"
)

// StoragePoolScanArgs is the river JobArgs for a `storage_pool.scan`
// task. The atomic-enqueue handler (Step 5) inserts the row, picks a
// fresh task id, and enqueues this payload via riverClient.InsertTx;
// the worker reads task / pool ids back to re-walk the saga it
// observed at hand-off.
type StoragePoolScanArgs struct {
	TaskID uuid.UUID `json:"task_id"`
	PoolID uuid.UUID `json:"pool_id"`
}

// Kind names the job kind. Mirrors the OpenAPI Task.type value
// surfaced through tasks.{list,get}.
func (StoragePoolScanArgs) Kind() string { return "storage_pool.scan" }

// ScanArgs is the per-task input the executor receives. Distinct from
// StoragePoolScanArgs (the river JobArgs): the worker resolves pool
// and node from the database before dispatching, so the executor sees
// a self-contained dependency-free struct.
//
// PoolName is the agent-side identifier the
// executor passes through `agentclient.PostScan` — the agent's pool
// registry is name-keyed. PoolID is preserved as the CP-side row id
// for logging / pressure-transition correlation.
type ScanArgs struct {
	PoolID             uuid.UUID
	PoolName           string
	NodeID             uuid.UUID
	AdvertisedEndpoint string
}

// ScanResult is the executor's output. Surfaces verbatim into
// storage_pools (via UpsertStoragePoolUsage) and into the task's
// `result` JSONB column. JSON tags are the wire form the worker
// marshals into tasks.result.
type ScanResult struct {
	CapacityBytes  int64     `json:"capacity_bytes"`
	AvailableBytes int64     `json:"available_bytes"`
	ReportedAt     time.Time `json:"reported_at"`
}

// ScanExecutor is the per-task-type seam. The production
// implementation is agentScanExecutor over the agentclient package;
// tests use a small in-package fake (`fakeScanExecutor`).
type ScanExecutor interface {
	Execute(ctx context.Context, args ScanArgs) (ScanResult, error)
}

// imageLister is the narrow agent-client surface the post-scan
// reconciliation step depends on. *agentclient.Client satisfies it
// structurally; tests pass a fake without importing the production
// client.
type imageLister interface {
	ListImages(ctx context.Context, endpoint string, poolName string) ([]agentapi.CachedImage, error)
}

// ScanDeps is the storage-pools-domain dependency bundle for river-job
// composition. Constructed inside BuildRiverClient.
//
// Executor and Lister are both optional: the worker is registered only
// when Executor is non-nil (skipped otherwise — useful for tests that
// build a river client purely for tasks.cancel's JobCancelTx); when
// Lister is nil, post-scan reconciliation is silently skipped
// (passive diagnostic).
type ScanDeps struct {
	Store    *store.Store
	Executor ScanExecutor
	Lister   imageLister
	Logger   *slog.Logger
	// PressureDisk pins the operator-configured pool-disk pressure
	// detection knobs. Honoured during
	// projectAndFinalize: every successful scan invokes the pure
	// computePoolDiskPressureTransition function against the scan
	// result + current pressure state and persists the next state in
	// the same transaction as UpsertStoragePoolUsage.
	PressureDisk config.PressureConditionConfig
}

// StoragePoolScanWorker is the lifecycle skeleton:
// transitions the tasks row through pending → running → terminal,
// delegates the actual scan to the executor, and projects the result
// into storage_pools via UpsertStoragePoolUsage in the same
// transaction as the terminal task update.
type StoragePoolScanWorker struct {
	river.WorkerDefaults[StoragePoolScanArgs]
	store        *store.Store
	executor     ScanExecutor
	lister       imageLister
	logger       *slog.Logger
	pressureDisk config.PressureConditionConfig
}

// Failure code constants for the task `error` JSONB envelope. New
// codes land alongside new failure branches.
const (
	errCodePoolNotFound = "pool_not_found"
	errCodeNodeNotFound = "node_not_found"
	errCodeScanFailed   = "scan_failed"
)

// Work executes one StoragePoolScanArgs job:
//
//  1. UpdateTaskRunning (pending → running, attempts++).
//  2. Resolve pool + node; failures → fail with the matching code.
//  3. Delegate to executor; failure → fail("scan_failed").
//  4. Project + finalize success in one transaction.
//
// Returning a non-nil error tells river to count this as an attempt
// and decide retry / discard against MaxAttempts. The task row's
// terminal status is already written by fail() before the return, so
// the OpenAPI Task projection is consistent regardless of what river
// does next.
func (w *StoragePoolScanWorker) Work(ctx context.Context, j *river.Job[StoragePoolScanArgs]) error {
	if w.executor == nil {
		return fmt.Errorf("scan executor not configured (Workers.Enabled likely false at boot)")
	}

	taskID := j.Args.TaskID
	poolID := j.Args.PoolID

	if err := w.store.Queries().UpdateTaskRunning(ctx, taskID); err != nil {
		return fmt.Errorf("update task running: %v", err)
	}

	pool, err := w.store.Queries().GetStoragePoolByID(ctx, poolID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return w.fail(ctx, taskID, errCodePoolNotFound,
				fmt.Errorf("storage pool %s not found", poolID))
		}
		return w.fail(ctx, taskID, errCodePoolNotFound,
			fmt.Errorf("load storage pool: %v", err))
	}

	node, err := w.store.Queries().GetNodeByID(ctx, pool.NodeID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return w.fail(ctx, taskID, errCodeNodeNotFound,
				fmt.Errorf("node %s not found", pool.NodeID))
		}
		return w.fail(ctx, taskID, errCodeNodeNotFound,
			fmt.Errorf("load node: %v", err))
	}

	result, execErr := w.executor.Execute(ctx, ScanArgs{
		PoolID:             pool.ID,
		PoolName:           pool.Name,
		NodeID:             node.ID,
		AdvertisedEndpoint: node.AdvertisedEndpoint,
	})
	if execErr != nil {
		return w.fail(ctx, taskID, errCodeScanFailed, execErr)
	}

	transition, err := w.projectAndFinalize(ctx, taskID, pool.ID, result)
	if err != nil {
		return err
	}
	w.logPoolPressureTransition(ctx, pool.ID, pool.Name, result, transition)

	// Passive post-scan reconciliation. Errors are logged but never
	// propagate - reconciliation is diagnostic, not a precondition
	// for scan task success.
	w.reconcileInventory(ctx, pool.ID, pool.Name, node.AdvertisedEndpoint)
	return nil
}

// logPoolPressureTransition emits one slog line per pool-disk pressure
// state change (set → WARN, clear → INFO). Steady-state and counting
// outcomes stay quiet. Logged after InTx commits so the line never
// appears for a rolled-back projection. Mirror of the heartbeat
// receiver's logPressureTransition.
func (w *StoragePoolScanWorker) logPoolPressureTransition(ctx context.Context, poolID uuid.UUID, poolName string, result ScanResult, kind poolPressureTransitionKind) {
	switch kind {
	case poolPressureTransitionSet:
		w.logger.WarnContext(ctx, "pool disk pressure set",
			"pool_id", poolID,
			"pool_name", poolName,
			"disk_percent", poolDiskPercent(result),
			"threshold_percent", w.pressureDisk.ThresholdPercent,
			"consecutive_required", w.pressureDisk.ConsecutiveRequired,
		)
	case poolPressureTransitionCleared:
		w.logger.InfoContext(ctx, "pool disk pressure cleared",
			"pool_id", poolID,
			"pool_name", poolName,
			"disk_percent", poolDiskPercent(result),
		)
	}
}

// poolDiskPercent returns the percentage available reported by the
// scan, or 0 when capacity is missing or zero. Diagnostic-only —
// pressure decision logic lives in computePoolDiskPressureTransition.
func poolDiskPercent(r ScanResult) float64 {
	if r.CapacityBytes <= 0 {
		return 0
	}
	return float64(r.AvailableBytes) * 100.0 / float64(r.CapacityBytes)
}

// projectAndFinalize writes the scan result into storage_pools, runs
// the pool-disk pressure transition, and
// transitions the task to success — all in one transaction. The
// agent-reported capacity / availability columns are
// owned by scan / heartbeat paths.
//
// Returns the pressure transition kind so the caller can emit a log
// line after the transaction commits (parallel pattern to the
// heartbeat receiver). Steady-state and counting outcomes are
// poolPressureTransitionNone and produce no log noise.
func (w *StoragePoolScanWorker) projectAndFinalize(
	ctx context.Context,
	taskID, poolID uuid.UUID,
	result ScanResult,
) (poolPressureTransitionKind, error) {
	resultJSON, err := marshalResult(result)
	if err != nil {
		return poolPressureTransitionNone, w.fail(ctx, taskID, errCodeScanFailed,
			fmt.Errorf("marshal scan result: %v", err))
	}

	cap := result.CapacityBytes
	avail := result.AvailableBytes
	transition := poolPressureTransitionNone
	txErr := w.store.InTx(ctx, func(q *store.Queries) error {
		if err := q.UpsertStoragePoolUsage(ctx, store.UpsertStoragePoolUsageParams{
			ID:             poolID,
			CapacityBytes:  &cap,
			AvailableBytes: &avail,
		}); err != nil {
			return fmt.Errorf("upsert storage pool usage: %v", err)
		}
		state, err := q.GetPoolDiskPressureState(ctx, poolID)
		if err != nil {
			return fmt.Errorf("read pool disk pressure state: %v", err)
		}
		newSince, newCount, kind := computePoolDiskPressureTransition(
			state.DiskPressureSince,
			state.DiskPressureCount,
			&avail,
			&cap,
			w.pressureDisk,
			time.Now().UTC(),
		)
		transition = kind
		if newSince != state.DiskPressureSince || newCount != state.DiskPressureCount {
			if err := q.UpdatePoolDiskPressure(ctx, store.UpdatePoolDiskPressureParams{
				ID:                poolID,
				DiskPressureSince: newSince,
				DiskPressureCount: newCount,
			}); err != nil {
				return fmt.Errorf("update pool disk pressure: %v", err)
			}
		}
		if err := q.UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{
			ID:     taskID,
			Status: store.TaskStatusSuccess,
			Result: resultJSON,
			Error:  nil,
		}); err != nil {
			return fmt.Errorf("finalize task: %v", err)
		}
		return nil
	})
	if txErr != nil {
		return poolPressureTransitionNone, txErr
	}
	return transition, nil
}

// fail marks the task as terminal-failed with a standard
// error envelope and returns cause so river decides retry vs discard.
// A failure to write the terminal row is itself returned with cause
// wrapped — visible in river's job error log; the task remains in
// `running`, and the next retry's UpdateTaskRunning is idempotent.
func (w *StoragePoolScanWorker) fail(
	ctx context.Context,
	taskID uuid.UUID,
	code string,
	cause error,
) error {
	envelope, marshalErr := marshalError(code, cause.Error())
	if marshalErr != nil {
		// Use a static envelope as last resort so the task row at least
		// surfaces as failed rather than wedged in running.
		envelope = []byte(`{"code":"internal","message":"marshal error envelope failed"}`)
		w.logger.ErrorContext(ctx, "storagepools.scan marshal error envelope failed",
			"task_id", taskID, "code", code, "error", marshalErr)
	}
	if err := w.store.Queries().UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{
		ID:     taskID,
		Status: store.TaskStatusFailed,
		Result: nil,
		Error:  envelope,
	}); err != nil {
		w.logger.ErrorContext(ctx, "storagepools.scan finalize-failed write failed",
			"task_id", taskID, "code", code, "error", err)
		return fmt.Errorf("finalize failed: %v (cause: %v)", err, cause)
	}
	return cause
}

// taskErrorJSON is the inner-error envelope written into tasks.error.
// Mirrors the public `Error.error` shape (code + message); details
// are not populated by task workers.
type taskErrorJSON struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// marshalResult serialises a ScanResult into the JSONB payload the
// OpenAPI Task.result field surfaces. ReportedAt is normalised to UTC
// so the wire form is timezone-stable.
func marshalResult(r ScanResult) ([]byte, error) {
	r.ReportedAt = r.ReportedAt.UTC()
	return json.Marshal(r)
}

// marshalError serialises a (code, message) pair into the JSONB
// payload the OpenAPI Task.error field surfaces.
func marshalError(code, message string) ([]byte, error) {
	return json.Marshal(taskErrorJSON{Code: code, Message: message})
}

// NewScanWorker constructs a StoragePoolScanWorker from the dependency
// bundle. Exported so integration tests can drive Work() directly
// (the "direct Worker.Work invocation against a fake" pattern)
// without going through the river client.
func NewScanWorker(deps ScanDeps) *StoragePoolScanWorker {
	return &StoragePoolScanWorker{
		store:        deps.Store,
		executor:     deps.Executor,
		lister:       deps.Lister,
		logger:       deps.Logger,
		pressureDisk: deps.PressureDisk,
	}
}

// ScanWorkers registers the storage-pools-domain scan worker on the
// bundle. Registration is unconditional — river requires the job
// kind to be in the workers registry for handler-side `Insert` to
// succeed even when the queue is never started (Workers.Enabled=false
// e2e contexts that need `tasks.cancel`'s JobCancelTx). When
// deps.Executor is nil the registered worker still answers Insert but
// errors out cleanly if it is ever fetched (see Work()).
func ScanWorkers(workers *river.Workers, deps ScanDeps) {
	river.AddWorker(workers, NewScanWorker(deps))
}

// reconciliationPageSize bounds the per-page walk of
// ListStorageImagesByPool used inside reconcileInventory. Reconciliation
// is an ops-time concern not a real-time one, so the cap is generous
// and not surfaced through the API.
const reconciliationPageSize int32 = 1000

// reconcileInventory compares the agent's authoritative cached-image
// list against the CP's storage_images projection for the pool and
// logs each discrepancy at WARN. Reconciliation is best-effort: any
// failure (agent unreachable, DB read error) is logged with the
// `scan_reconciliation_failed` message and swallowed, so the parent
// scan task remains terminal-success.
//
// A nil lister silently skips reconciliation — the worker is
// configured without an agent client (test contexts, workers-disabled
// mode), and the passive-diagnostic stance means nothing is logged.
func (w *StoragePoolScanWorker) reconcileInventory(ctx context.Context, poolID uuid.UUID, poolName, agentEndpoint string) {
	if w.lister == nil {
		return
	}
	agentInventory, err := w.lister.ListImages(ctx, agentEndpoint, poolName)
	if err != nil {
		w.logger.WarnContext(ctx, "scan_reconciliation_failed",
			"pool_id", poolID, "error", err)
		return
	}
	cpRows, err := w.loadAllStorageImages(ctx, poolID)
	if err != nil {
		w.logger.WarnContext(ctx, "scan_reconciliation_failed",
			"pool_id", poolID, "error", err)
		return
	}
	diffAndLogInventory(ctx, w.logger, poolID, agentInventory, cpRows)
}

// loadAllStorageImages walks ListStorageImagesByPool with cursor
// pagination until the agent's inventory size has been matched in
// page-sized batches. The cursor is encoded as the last row's
// (imported_at, id), mirroring the read-handler's cursor shape.
func (w *StoragePoolScanWorker) loadAllStorageImages(ctx context.Context, poolID uuid.UUID) ([]store.StorageImage, error) {
	var (
		all      []store.StorageImage
		cursorAt *time.Time
		cursorID *uuid.UUID
	)
	for {
		page, err := w.store.Queries().ListStorageImagesByPool(ctx, store.ListStorageImagesByPoolParams{
			PoolID:           poolID,
			CursorImportedAt: cursorAt,
			CursorID:         cursorID,
			LimitCount:       reconciliationPageSize,
		})
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < int(reconciliationPageSize) {
			return all, nil
		}
		last := page[len(page)-1]
		cursorAt = &last.ImportedAt
		cursorID = &last.ID
	}
}

// diffAndLogInventory keys both sides on `checksum_sha256` and emits
// one WARN line per discrepancy:
//
//   - `storage_image_orphan_on_agent` — sha256 present on agent, absent
//     in CP. Common cause: prior failed import that committed on agent
//     but did not project on CP.
//   - `storage_image_missing_on_agent` — sha256 present in CP, absent
//     on agent. Common cause: agent-side out-of-band cleanup or
//     filesystem mutation.
//
// Pure function for ease of unit testing — no receivers, no I/O.
func diffAndLogInventory(
	ctx context.Context,
	log *slog.Logger,
	poolID uuid.UUID,
	agentInventory []agentapi.CachedImage,
	cpRows []store.StorageImage,
) {
	agentSet := make(map[string]agentapi.CachedImage, len(agentInventory))
	for _, img := range agentInventory {
		agentSet[img.ChecksumSha256] = img
	}
	cpSet := make(map[string]store.StorageImage, len(cpRows))
	for _, row := range cpRows {
		cpSet[row.ChecksumSha256] = row
	}
	for sha, img := range agentSet {
		if _, ok := cpSet[sha]; !ok {
			log.WarnContext(ctx, "storage_image_orphan_on_agent",
				"pool_id", poolID,
				"sha256", sha,
				"size_bytes", img.SizeBytes,
				"agent_path", img.Path)
		}
	}
	for sha, row := range cpSet {
		if _, ok := agentSet[sha]; !ok {
			log.WarnContext(ctx, "storage_image_missing_on_agent",
				"pool_id", poolID,
				"image_id", row.ID,
				"sha256", sha)
		}
	}
}
