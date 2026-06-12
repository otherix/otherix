// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/store"
)

// Run-form storage-pool workers for the etcd job runtime. They
// reuse the executor seams, Args payloads, error classifiers, and pressure
// computation of the former workers but drive the orchestration against the
// WorkerStore interface (the etcd store's task mutators + entity reads + atomic
// projections).
//
// The scan worker's passive post-scan inventory reconciliation (orphan/missing
// image diagnostics) is intentionally omitted here; it is best-effort logging,
// not part of the task outcome, and is wired separately when the etcd runtime
// gains the agent-lister dependency.

// WorkerStore is the storage surface the storage-pool worker handlers depend on.
// *etcdstore.Store satisfies it (asserted in the etcdstore integration tests).
type WorkerStore interface {
	UpdateTaskRunning(ctx context.Context, id uuid.UUID) (alreadyTerminal bool, err error)
	UpdateTaskFinalized(ctx context.Context, arg store.UpdateTaskFinalizedParams) error
	StoragePoolByID(ctx context.Context, id uuid.UUID) (store.StoragePool, error)
	NodeByID(ctx context.Context, id uuid.UUID) (store.Node, error)
	ProjectStoragePoolScan(ctx context.Context, usage store.UpsertStoragePoolUsageParams, pressure store.UpdatePoolDiskPressureParams, fin store.UpdateTaskFinalizedParams) error
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
	alreadyTerminal, err := st.UpdateTaskRunning(ctx, taskID)
	if err != nil {
		return fmt.Errorf("update task running: %v", err)
	}
	if alreadyTerminal {
		// The task already committed success/cancelled (a lost-ACK redelivery, or
		// a cancel that won the CAS): do NOT contact the agent. Return nil so the
		// dispatcher CompleteJob-deletes the job.
		return nil
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
