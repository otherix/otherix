// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/store"
)

// StorageImageImportArgs is the river JobArgs for a
// `storage_image.import` task. The atomic-enqueue handler (Step 10
// templates/image_import.go) inserts the row, mints a fresh task id,
// and enqueues this payload via riverClient.InsertTx; the worker
// resolves template / pool / node from the database before
// dispatching to the executor.
type StorageImageImportArgs struct {
	TaskID     uuid.UUID `json:"task_id"`
	TemplateID uuid.UUID `json:"template_id"`
	PoolID     uuid.UUID `json:"pool_id"`
}

// Kind names the job kind. Mirrors the OpenAPI Task.type value
// surfaced through tasks.{list,get}.
func (StorageImageImportArgs) Kind() string { return "storage_image.import" }

// ImportArgs is the per-task input the executor receives. Distinct
// from StorageImageImportArgs (the river JobArgs): the worker
// resolves template, pool, and node before dispatching, so the
// executor sees a self-contained dependency-free struct.
//
// AgentTaskID carries the persisted agent-side task uuid for
// resumption - nil on first run, non-nil when the worker is
// recovering from a CP-side restart.
//
// OnAgentTaskID is the callback the executor invokes immediately
// after the agent's 202 returns, persisting the agent task id via
// UpdateTaskAgentTaskID. The callback shape keeps the executor
// seam narrow - the executor never touches
// *store.Store directly.
type ImportArgs struct {
	TaskID        uuid.UUID
	AgentTaskID   *uuid.UUID
	Pool          store.StoragePool
	Node          store.Node
	Template      store.Template
	OnAgentTaskID func(ctx context.Context, agentTaskID uuid.UUID) error
}

// ImportResult is the executor's output. Surfaces verbatim into
// storage_images (via CreateStorageImage's ON CONFLICT DO UPDATE)
// and into the task's `result` JSONB column. JSON tags are the wire
// form the worker marshals into tasks.result.
type ImportResult struct {
	ChecksumSHA256 string `json:"checksum_sha256"`
	SizeBytes      int64  `json:"size_bytes"`
	Format         string `json:"format"`
}

// ImportExecutor is the per-task-type seam. The
// production implementation (agentImportExecutor) lives in this
// package; tests pass an in-test fake.
type ImportExecutor interface {
	Execute(ctx context.Context, args ImportArgs) (ImportResult, error)
}

// ImportDeps is the storage-image-import dependency bundle for
// river-job composition. Constructed inside BuildRiverClient.
// Executor is optional: when nil, the worker is not registered
// (relevant for tests building a river client purely for
// tasks.cancel's JobCancelTx).
type ImportDeps struct {
	Store    *store.Store
	Executor ImportExecutor
	Logger   *slog.Logger
}

// StorageImageImportWorker is the lifecycle skeleton
// for storage_image.import: transitions the tasks row through
// pending → running → terminal, delegates the agent-saga to the
// executor, and projects the result into storage_images via
// CreateStorageImage in the same transaction as the terminal task
// update.
type StorageImageImportWorker struct {
	river.WorkerDefaults[StorageImageImportArgs]
	store    *store.Store
	executor ImportExecutor
	logger   *slog.Logger
}

// Failure code constants for the task `error` envelope. Passthrough
// codes (qcow2_header_invalid, checksum_mismatch, etc.) come from
// the agent envelope and are preserved verbatim by
// classifyImportError.
const (
	errCodeImportTemplateNotFound = "template_not_found"
	errCodeImportPoolNotFound     = "pool_not_found"
	errCodeImportNodeNotFound     = "node_not_found"
	errCodeImportAgentUnreachable = "agent_unreachable"
	errCodeImportTimeout          = "import_timeout"
	errCodeImportInternal         = "internal"
)

// Work executes one StorageImageImportArgs job:
//
//  1. UpdateTaskRunning (pending → running, attempts++).
//  2. Resolve template / pool / node; failures → fail with the
//     matching `template_not_found` / `pool_not_found` /
//     `node_not_found` code.
//  3. Re-load the task to inspect agent_task_id (resumption seam:
//     non-nil means a previous attempt already POSTed к agent).
//  4. Delegate to executor with an OnAgentTaskID callback that
//     persists the agent task id back through UpdateTaskAgentTaskID
//     on the first 202.
//  5. Project + finalize success in one transaction
//     (CreateStorageImage ON CONFLICT DO UPDATE +
//     UpdateTaskFinalized).
//
// Returning a non-nil error tells river to count this as an attempt
// and decide retry / discard against MaxAttempts. The task row's
// terminal status is already written by fail() before the return,
// so the OpenAPI Task projection is consistent regardless of what
// river does next.
func (w *StorageImageImportWorker) Work(ctx context.Context, j *river.Job[StorageImageImportArgs]) error {
	if w.executor == nil {
		return fmt.Errorf("import executor not configured (Workers.Enabled likely false at boot)")
	}

	taskID := j.Args.TaskID

	if err := w.store.Queries().UpdateTaskRunning(ctx, taskID); err != nil {
		return fmt.Errorf("update task running: %v", err)
	}

	tpl, pool, node, err := w.loadEntities(ctx, taskID, j.Args.TemplateID, j.Args.PoolID)
	if err != nil {
		return err
	}

	task, err := w.store.Queries().GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("reload task: %v", err)
	}

	result, execErr := w.executor.Execute(ctx, ImportArgs{
		TaskID:        taskID,
		AgentTaskID:   task.AgentTaskID,
		Pool:          pool,
		Node:          node,
		Template:      tpl,
		OnAgentTaskID: w.persistAgentTaskID(taskID),
	})
	if execErr != nil {
		return w.fail(ctx, taskID, classifyImportError(execErr), execErr)
	}

	return w.projectAndFinalize(ctx, taskID, j.Args.TemplateID, j.Args.PoolID, result)
}

// loadEntities resolves the three FK targets the executor needs.
// Each lookup classifies its own ErrNoRows to the matching error
// code. Returns a non-nil error already passed through fail() so
// the caller can return it verbatim.
func (w *StorageImageImportWorker) loadEntities(
	ctx context.Context,
	taskID, templateID, poolID uuid.UUID,
) (store.Template, store.StoragePool, store.Node, error) {
	tpl, err := w.store.Queries().GetTemplate(ctx, templateID)
	if err != nil {
		failCode := errCodeImportTemplateNotFound
		if !errors.Is(err, pgx.ErrNoRows) {
			failCode = errCodeImportInternal
		}
		return store.Template{}, store.StoragePool{}, store.Node{},
			w.fail(ctx, taskID, failCode, fmt.Errorf("load template: %v", err))
	}
	pool, err := w.store.Queries().GetStoragePoolByID(ctx, poolID)
	if err != nil {
		failCode := errCodeImportPoolNotFound
		if !errors.Is(err, pgx.ErrNoRows) {
			failCode = errCodeImportInternal
		}
		return store.Template{}, store.StoragePool{}, store.Node{},
			w.fail(ctx, taskID, failCode, fmt.Errorf("load pool: %v", err))
	}
	node, err := w.store.Queries().GetNodeByID(ctx, pool.NodeID)
	if err != nil {
		failCode := errCodeImportNodeNotFound
		if !errors.Is(err, pgx.ErrNoRows) {
			failCode = errCodeImportInternal
		}
		return store.Template{}, store.StoragePool{}, store.Node{},
			w.fail(ctx, taskID, failCode, fmt.Errorf("load node: %v", err))
	}
	return tpl, pool, node, nil
}

// persistAgentTaskID returns the closure the executor invokes after
// the agent's 202 returns. UpdateTaskAgentTaskID is the dedicated
// one-row UPDATE that backs the resumption seam.
func (w *StorageImageImportWorker) persistAgentTaskID(taskID uuid.UUID) func(context.Context, uuid.UUID) error {
	return func(ctx context.Context, agentTaskID uuid.UUID) error {
		return w.store.Queries().UpdateTaskAgentTaskID(ctx, store.UpdateTaskAgentTaskIDParams{
			ID:          taskID,
			AgentTaskID: &agentTaskID,
		})
	}
}

// projectAndFinalize writes the import result into storage_images
// and transitions the task to success in one transaction. The
// CreateStorageImage query carries ON CONFLICT (template_id,
// pool_id) DO UPDATE per spec TR7 — a re-import with rotated
// content overwrites the row's checksum / size / format.
//
// Back-propagation: when the template row carries a NULL
// image_checksum_sha256 (compute-mode registration), the worker
// folds the agent-computed checksum onto the template row in the
// same transaction. UpdateTemplateImageChecksumIfNull is idempotent
// (its WHERE clause guards on `image_checksum_sha256 is null`), so
// two concurrent compute-mode imports cannot conflict and а repeat
// import after back-propagation is а no-op. After this point the
// template behaves as if verify mode had been used originally —
// future imports k other pools hit the agent с а known
// expected_checksum_sha256.
func (w *StorageImageImportWorker) projectAndFinalize(
	ctx context.Context,
	taskID, templateID, poolID uuid.UUID,
	result ImportResult,
) error {
	resultJSON, err := marshalImportResult(result)
	if err != nil {
		return w.fail(ctx, taskID, errCodeImportInternal,
			fmt.Errorf("marshal import result: %v", err))
	}
	checksumBytes, err := hex.DecodeString(result.ChecksumSHA256)
	if err != nil {
		return w.fail(ctx, taskID, errCodeImportInternal,
			fmt.Errorf("decode result checksum: %v", err))
	}

	return w.store.InTx(ctx, func(q *store.Queries) error {
		if _, err := q.CreateStorageImage(ctx, store.CreateStorageImageParams{
			ID:             uuid.New(),
			TemplateID:     templateID,
			PoolID:         poolID,
			ChecksumSha256: result.ChecksumSHA256,
			SizeBytes:      result.SizeBytes,
			Format:         result.Format,
		}); err != nil {
			return fmt.Errorf("create storage image: %v", err)
		}
		if err := q.UpdateTemplateImageChecksumIfNull(ctx, store.UpdateTemplateImageChecksumIfNullParams{
			ID:                  templateID,
			ImageChecksumSha256: checksumBytes,
		}); err != nil {
			return fmt.Errorf("back-propagate template checksum: %v", err)
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
}

// fail mirrors StoragePoolScanWorker.fail — writes a terminal
// failed envelope with code + cause.Error() and returns cause so
// river decides retry vs discard against MaxAttempts.
func (w *StorageImageImportWorker) fail(
	ctx context.Context,
	taskID uuid.UUID,
	code string,
	cause error,
) error {
	envelope, marshalErr := marshalError(code, cause.Error())
	if marshalErr != nil {
		envelope = []byte(`{"code":"internal","message":"marshal error envelope failed"}`)
		w.logger.ErrorContext(ctx, "storagepools.import marshal error envelope failed",
			"task_id", taskID, "code", code, "error", marshalErr)
	}
	if err := w.store.Queries().UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{
		ID:     taskID,
		Status: store.TaskStatusFailed,
		Result: nil,
		Error:  envelope,
	}); err != nil {
		w.logger.ErrorContext(ctx, "storagepools.import finalize-failed write failed",
			"task_id", taskID, "code", code, "error", err)
		return fmt.Errorf("finalize failed: %v (cause: %v)", err, cause)
	}
	return cause
}

// classifyImportError maps an executor error to a tasks.error.code.
// Agent envelope passthrough preserves agent-side codes verbatim
// (qcow2_header_invalid, checksum_mismatch, pool_full, …). Network
// / 5xx failures collapse to `agent_unreachable`; poll-budget
// exhaustion collapses to `import_timeout`; everything else falls
// through to `internal`.
func classifyImportError(err error) string {
	var ae *agentclient.AgentError
	if errors.As(err, &ae) {
		if ae.Code != "" {
			return ae.Code
		}
		return errCodeImportAgentUnreachable
	}
	var te *agentclient.TimeoutError
	if errors.As(err, &te) {
		return errCodeImportTimeout
	}
	return errCodeImportInternal
}

// marshalImportResult serialises an ImportResult into the JSONB
// payload the OpenAPI Task.result field surfaces.
func marshalImportResult(r ImportResult) ([]byte, error) {
	return json.Marshal(r)
}

// NewImportWorker constructs a StorageImageImportWorker from the
// dependency bundle. Exported so direct Worker.Work invocation in
// tests works without going through the river client.
func NewImportWorker(deps ImportDeps) *StorageImageImportWorker {
	return &StorageImageImportWorker{
		store:    deps.Store,
		executor: deps.Executor,
		logger:   deps.Logger,
	}
}

// ImportWorkers registers the storage-image-import worker on the
// bundle. Registration is unconditional — river requires the job
// kind to be in the workers registry for handler-side `Insert` to
// succeed even when the queue is never started. When deps.Executor
// is nil the registered worker still answers Insert but errors out
// cleanly if it is ever fetched (see Work()).
func ImportWorkers(workers *river.Workers, deps ImportDeps) {
	river.AddWorker(workers, NewImportWorker(deps))
}
