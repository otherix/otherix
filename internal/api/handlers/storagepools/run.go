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
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/store"
)

// Backend-agnostic Run-form storage-pool workers for the etcd job runtime. They
// reuse the executor seams, Args payloads, error classifiers, and pressure
// computation of the river workers but drive the orchestration against the
// WorkerStore interface (the etcd store's task mutators + entity reads + atomic
// projections) instead of pgx + InTx.
//
// The scan worker's passive post-scan inventory reconciliation (orphan/missing
// image diagnostics) is intentionally omitted here; it is best-effort logging,
// not part of the task outcome, and is wired separately when the etcd runtime
// gains the agent-lister dependency.

// WorkerStore is the storage surface the storage-pool worker handlers depend on.
// *etcdstore.Store satisfies it (asserted in the etcdstore integration tests).
type WorkerStore interface {
	UpdateTaskRunning(ctx context.Context, id uuid.UUID) error
	UpdateTaskFinalized(ctx context.Context, arg store.UpdateTaskFinalizedParams) error
	UpdateTaskAgentTaskID(ctx context.Context, arg store.UpdateTaskAgentTaskIDParams) error
	TaskByID(ctx context.Context, id uuid.UUID) (store.Task, error)
	StoragePoolByID(ctx context.Context, id uuid.UUID) (store.StoragePool, error)
	NodeByID(ctx context.Context, id uuid.UUID) (store.Node, error)
	TemplateByID(ctx context.Context, id uuid.UUID) (store.Template, error)
	ProjectStoragePoolScan(ctx context.Context, usage store.UpsertStoragePoolUsageParams, pressure store.UpdatePoolDiskPressureParams, fin store.UpdateTaskFinalizedParams) error
	ProjectStorageImageImport(ctx context.Context, img store.CreateStorageImageParams, tmplChecksum store.UpdateTemplateImageChecksumIfNullParams, fin store.UpdateTaskFinalizedParams) error
}

// ScanHandler returns the dispatcher handler for storage_pool.scan jobs.
func ScanHandler(st WorkerStore, exec ScanExecutor, pressureDisk config.PressureConditionConfig, log *slog.Logger) func(context.Context, []byte) error {
	return func(ctx context.Context, raw []byte) error {
		var args StoragePoolScanArgs
		if err := json.Unmarshal(raw, &args); err != nil {
			return fmt.Errorf("unmarshal storage_pool.scan args: %v", err)
		}
		return runScan(ctx, st, exec, pressureDisk, log, args)
	}
}

func runScan(ctx context.Context, st WorkerStore, exec ScanExecutor, pressureDisk config.PressureConditionConfig, log *slog.Logger, args StoragePoolScanArgs) error {
	taskID := args.TaskID
	if err := st.UpdateTaskRunning(ctx, taskID); err != nil {
		return fmt.Errorf("update task running: %v", err)
	}
	pool, err := st.StoragePoolByID(ctx, args.PoolID)
	if err != nil {
		return failTask(ctx, st, log, "storagepools.scan", taskID, errCodePoolNotFound, fmt.Errorf("load storage pool: %v", err))
	}
	node, err := st.NodeByID(ctx, pool.NodeID)
	if err != nil {
		return failTask(ctx, st, log, "storagepools.scan", taskID, errCodeNodeNotFound, fmt.Errorf("load node: %v", err))
	}

	result, execErr := exec.Execute(ctx, ScanArgs{
		PoolID:             pool.ID,
		PoolName:           pool.Name,
		NodeID:             node.ID,
		AdvertisedEndpoint: node.AdvertisedEndpoint,
	})
	if execErr != nil {
		return failTask(ctx, st, log, "storagepools.scan", taskID, errCodeScanFailed, execErr)
	}

	resultJSON, err := marshalResult(result)
	if err != nil {
		return failTask(ctx, st, log, "storagepools.scan", taskID, errCodeScanFailed, fmt.Errorf("marshal scan result: %v", err))
	}
	capacity := result.CapacityBytes
	available := result.AvailableBytes
	newSince, newCount, _ := computePoolDiskPressureTransition(
		pool.DiskPressureSince, pool.DiskPressureCount, &available, &capacity, pressureDisk, time.Now().UTC(),
	)
	if err := st.ProjectStoragePoolScan(ctx,
		store.UpsertStoragePoolUsageParams{ID: pool.ID, CapacityBytes: &capacity, AvailableBytes: &available},
		store.UpdatePoolDiskPressureParams{ID: pool.ID, DiskPressureSince: newSince, DiskPressureCount: newCount},
		store.UpdateTaskFinalizedParams{ID: taskID, Status: store.TaskStatusSuccess, Result: resultJSON},
	); err != nil {
		return fmt.Errorf("project scan success: %v", err)
	}
	return nil
}

// ImportHandler returns the dispatcher handler for storage_image.import jobs.
func ImportHandler(st WorkerStore, exec ImportExecutor, log *slog.Logger) func(context.Context, []byte) error {
	return func(ctx context.Context, raw []byte) error {
		var args StorageImageImportArgs
		if err := json.Unmarshal(raw, &args); err != nil {
			return fmt.Errorf("unmarshal storage_image.import args: %v", err)
		}
		return runImport(ctx, st, exec, log, args)
	}
}

func runImport(ctx context.Context, st WorkerStore, exec ImportExecutor, log *slog.Logger, args StorageImageImportArgs) error {
	taskID := args.TaskID
	if err := st.UpdateTaskRunning(ctx, taskID); err != nil {
		return fmt.Errorf("update task running: %v", err)
	}
	tpl, err := st.TemplateByID(ctx, args.TemplateID)
	if err != nil {
		return failTask(ctx, st, log, "storagepools.import", taskID, classifyImportLoad(err, errCodeImportTemplateNotFound), fmt.Errorf("load template: %v", err))
	}
	pool, err := st.StoragePoolByID(ctx, args.PoolID)
	if err != nil {
		return failTask(ctx, st, log, "storagepools.import", taskID, classifyImportLoad(err, errCodeImportPoolNotFound), fmt.Errorf("load pool: %v", err))
	}
	node, err := st.NodeByID(ctx, pool.NodeID)
	if err != nil {
		return failTask(ctx, st, log, "storagepools.import", taskID, classifyImportLoad(err, errCodeImportNodeNotFound), fmt.Errorf("load node: %v", err))
	}
	task, err := st.TaskByID(ctx, taskID)
	if err != nil {
		return fmt.Errorf("reload task: %v", err)
	}

	result, execErr := exec.Execute(ctx, ImportArgs{
		TaskID:        taskID,
		AgentTaskID:   task.AgentTaskID,
		Pool:          pool,
		Node:          node,
		Template:      tpl,
		OnAgentTaskID: onAgentTaskID(st, taskID),
	})
	if execErr != nil {
		return failTask(ctx, st, log, "storagepools.import", taskID, classifyImportError(execErr), execErr)
	}

	resultJSON, err := marshalImportResult(result)
	if err != nil {
		return failTask(ctx, st, log, "storagepools.import", taskID, errCodeImportInternal, fmt.Errorf("marshal import result: %v", err))
	}
	checksumBytes, err := hex.DecodeString(result.ChecksumSHA256)
	if err != nil {
		return failTask(ctx, st, log, "storagepools.import", taskID, errCodeImportInternal, fmt.Errorf("decode result checksum: %v", err))
	}
	if err := st.ProjectStorageImageImport(ctx,
		store.CreateStorageImageParams{
			ID: uuid.New(), TemplateID: args.TemplateID, PoolID: args.PoolID,
			ChecksumSha256: result.ChecksumSHA256, SizeBytes: result.SizeBytes, Format: result.Format,
		},
		store.UpdateTemplateImageChecksumIfNullParams{ID: args.TemplateID, ImageChecksumSha256: checksumBytes},
		store.UpdateTaskFinalizedParams{ID: taskID, Status: store.TaskStatusSuccess, Result: resultJSON},
	); err != nil {
		return fmt.Errorf("project import success: %v", err)
	}
	return nil
}

// onAgentTaskID returns the resumption callback persisting the agent task id.
func onAgentTaskID(st WorkerStore, taskID uuid.UUID) func(context.Context, uuid.UUID) error {
	return func(ctx context.Context, agentTaskID uuid.UUID) error {
		return st.UpdateTaskAgentTaskID(ctx, store.UpdateTaskAgentTaskIDParams{ID: taskID, AgentTaskID: &agentTaskID})
	}
}

// classifyImportLoad maps an entity-read error to a *_not_found code (absent
// row) or the import internal code otherwise.
func classifyImportLoad(err error, notFoundCode string) string {
	if errors.Is(err, store.ErrNotFound) {
		return notFoundCode
	}
	return errCodeImportInternal
}

// failTask writes the terminal failed envelope and returns cause so the
// dispatcher requeues vs fails against the kind's attempt budget. Mirrors the
// river workers' fail() against the WorkerStore mutator.
func failTask(ctx context.Context, st WorkerStore, log *slog.Logger, op string, taskID uuid.UUID, code string, cause error) error {
	envelope, marshalErr := marshalError(code, cause.Error())
	if marshalErr != nil {
		envelope = []byte(`{"code":"internal","message":"marshal error envelope failed"}`)
		log.ErrorContext(ctx, op+" marshal error envelope failed", "task_id", taskID, "code", code, "error", marshalErr)
	}
	if err := st.UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{ID: taskID, Status: store.TaskStatusFailed, Error: envelope}); err != nil {
		log.ErrorContext(ctx, op+" finalize-failed write failed", "task_id", taskID, "code", code, "error", err)
		return fmt.Errorf("finalize failed: %v (cause: %v)", err, cause)
	}
	return cause
}
