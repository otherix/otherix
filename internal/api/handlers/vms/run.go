// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// Backend-agnostic Run-form async VM workers for the etcd job runtime. These
// reuse the executor seams, Args payloads, and error classifiers of the river
// workers but drive the orchestration against the WorkerStore interface (the
// etcd store's task mutators + entity reads + atomic projections) instead of
// pgx + InTx. The handler constructors return functions assignable to
// worker.Handler; cmd/api registers them on the dispatcher.

// WorkerStore is the storage surface the async VM worker handlers depend on:
// task-lifecycle mutators, entity reads, and the atomic success projections.
// *etcdstore.Store satisfies it (asserted in the etcdstore integration tests).
type WorkerStore interface {
	UpdateTaskRunning(ctx context.Context, id uuid.UUID) error
	UpdateTaskFinalized(ctx context.Context, arg store.UpdateTaskFinalizedParams) error
	UpdateTaskAgentTaskID(ctx context.Context, arg store.UpdateTaskAgentTaskIDParams) error
	TaskByID(ctx context.Context, id uuid.UUID) (store.Task, error)
	VMByID(ctx context.Context, id uuid.UUID) (store.VM, error)
	ListVMDisksByVM(ctx context.Context, vmID uuid.UUID) ([]store.VMDisk, error)
	TemplateByID(ctx context.Context, id uuid.UUID) (store.Template, error)
	StoragePoolByID(ctx context.Context, id uuid.UUID) (store.StoragePool, error)
	NodeByID(ctx context.Context, id uuid.UUID) (store.Node, error)
	ProjectVMCreateSuccess(ctx context.Context, rt store.UpsertVMRuntimeParams, templateID uuid.UUID, fin store.UpdateTaskFinalizedParams) error
	ProjectVMDeleteSuccess(ctx context.Context, vm store.VM, fin store.UpdateTaskFinalizedParams) error
	ProjectVMLifecycleSuccess(ctx context.Context, vmID uuid.UUID, desiredPhase store.VMDesiredPhase, runtimePhase store.VMPhase, fin store.UpdateTaskFinalizedParams) error
}

// CreateHandler returns the dispatcher handler for vm.create jobs.
func CreateHandler(st WorkerStore, exec CreateExecutor, log *slog.Logger) func(context.Context, []byte) error {
	return func(ctx context.Context, raw []byte) error {
		var args VMCreateArgs
		if err := json.Unmarshal(raw, &args); err != nil {
			return fmt.Errorf("unmarshal vm.create args: %v", err)
		}
		return runCreate(ctx, st, exec, log, args)
	}
}

func runCreate(ctx context.Context, st WorkerStore, exec CreateExecutor, log *slog.Logger, args VMCreateArgs) error {
	taskID := args.TaskID
	if err := st.UpdateTaskRunning(ctx, taskID); err != nil {
		return fmt.Errorf("update task running: %v", err)
	}
	vm, err := st.VMByID(ctx, args.VMID)
	if err != nil {
		return failRun(ctx, st, log, "vms.create", taskID, classifyLoadErr(err, errCodeVMNotFound), fmt.Errorf("load vm: %v", err))
	}
	disks, err := st.ListVMDisksByVM(ctx, args.VMID)
	if err != nil {
		return failRun(ctx, st, log, "vms.create", taskID, "internal", fmt.Errorf("list vm disks: %v", err))
	}
	if len(disks) == 0 {
		return failRun(ctx, st, log, "vms.create", taskID, "internal", fmt.Errorf("vm %s has no disks", args.VMID))
	}
	tpl, err := st.TemplateByID(ctx, args.TemplateID)
	if err != nil {
		return failRun(ctx, st, log, "vms.create", taskID, classifyLoadErr(err, errCodeVMTemplateMissing), fmt.Errorf("load template: %v", err))
	}
	pool, err := st.StoragePoolByID(ctx, args.PoolID)
	if err != nil {
		return failRun(ctx, st, log, "vms.create", taskID, classifyLoadErr(err, errCodeVMPoolMissing), fmt.Errorf("load pool: %v", err))
	}
	node, err := st.NodeByID(ctx, args.NodeID)
	if err != nil {
		return failRun(ctx, st, log, "vms.create", taskID, classifyLoadErr(err, errCodeVMNodeMissing), fmt.Errorf("load node: %v", err))
	}
	task, err := st.TaskByID(ctx, taskID)
	if err != nil {
		return fmt.Errorf("reload task: %v", err)
	}

	result, execErr := exec.Execute(ctx, CreateArgs{
		TaskID:        taskID,
		AgentTaskID:   task.AgentTaskID,
		VM:            vm,
		Disk:          disks[0],
		Template:      tpl,
		Pool:          pool,
		Node:          node,
		OnAgentTaskID: onAgentTaskID(st, taskID),
	})
	if execErr != nil {
		return failRun(ctx, st, log, "vms.create", taskID, classifyVMError(execErr, errCodeVMCreateFailed), execErr)
	}

	resultJSON, err := json.Marshal(result)
	if err != nil {
		return failRun(ctx, st, log, "vms.create", taskID, "internal", fmt.Errorf("marshal create result: %v", err))
	}
	nodeID := args.NodeID
	if err := st.ProjectVMCreateSuccess(ctx,
		store.UpsertVMRuntimeParams{VmID: args.VMID, CurrentNodeID: &nodeID, Phase: store.VmPhaseRunning, ObservedGeneration: 1},
		args.TemplateID,
		store.UpdateTaskFinalizedParams{ID: taskID, Status: store.TaskStatusSuccess, Result: resultJSON},
	); err != nil {
		return fmt.Errorf("project create success: %v", err)
	}
	return nil
}

// DeleteHandler returns the dispatcher handler for vm.delete jobs.
func DeleteHandler(st WorkerStore, exec DeleteExecutor, log *slog.Logger) func(context.Context, []byte) error {
	return func(ctx context.Context, raw []byte) error {
		var args VMDeleteArgs
		if err := json.Unmarshal(raw, &args); err != nil {
			return fmt.Errorf("unmarshal vm.delete args: %v", err)
		}
		return runDelete(ctx, st, exec, log, args)
	}
}

func runDelete(ctx context.Context, st WorkerStore, exec DeleteExecutor, log *slog.Logger, args VMDeleteArgs) error {
	taskID := args.TaskID
	if err := st.UpdateTaskRunning(ctx, taskID); err != nil {
		return fmt.Errorf("update task running: %v", err)
	}
	vm, err := st.VMByID(ctx, args.VMID)
	if err != nil {
		return failRun(ctx, st, log, "vms.delete", taskID, classifyLoadErr(err, errCodeVMNotFound), fmt.Errorf("load vm: %v", err))
	}
	node, err := st.NodeByID(ctx, args.NodeID)
	if err != nil {
		return failRun(ctx, st, log, "vms.delete", taskID, classifyLoadErr(err, errCodeVMNodeMissing), fmt.Errorf("load node: %v", err))
	}
	task, err := st.TaskByID(ctx, taskID)
	if err != nil {
		return fmt.Errorf("reload task: %v", err)
	}

	result, execErr := exec.Execute(ctx, DeleteArgs{
		TaskID:        taskID,
		AgentTaskID:   task.AgentTaskID,
		VMID:          args.VMID,
		VMName:        vm.Name,
		Node:          node,
		OnAgentTaskID: onAgentTaskID(st, taskID),
	})
	if execErr != nil {
		return failRun(ctx, st, log, "vms.delete", taskID, classifyVMError(execErr, errCodeVMDeleteFailed), execErr)
	}

	resultJSON, err := json.Marshal(result)
	if err != nil {
		return failRun(ctx, st, log, "vms.delete", taskID, "internal", fmt.Errorf("marshal delete result: %v", err))
	}
	if err := st.ProjectVMDeleteSuccess(ctx, vm,
		store.UpdateTaskFinalizedParams{ID: taskID, Status: store.TaskStatusSuccess, Result: resultJSON},
	); err != nil {
		return fmt.Errorf("project delete success: %v", err)
	}
	return nil
}

// LifecycleHandler returns the dispatcher handler for one of the async lifecycle
// kinds (vm.start / vm.stop / vm.poweroff / vm.reboot). op is the agent-side
// action segment; desiredPhase is written on success (empty = unchanged, e.g.
// reboot); runtimePhase is the observed phase projected through.
func LifecycleHandler(st WorkerStore, exec LifecycleExecutor, log *slog.Logger, op string, desiredPhase store.VMDesiredPhase, runtimePhase store.VMPhase, failureCode string) func(context.Context, []byte) error {
	return func(ctx context.Context, raw []byte) error {
		var args VMStartArgs // every lifecycle Args shares {task_id, vm_id, node_id}
		if err := json.Unmarshal(raw, &args); err != nil {
			return fmt.Errorf("unmarshal vm.%s args: %v", op, err)
		}
		return runLifecycle(ctx, st, exec, log, args.TaskID, args.VMID, args.NodeID, op, desiredPhase, runtimePhase, failureCode)
	}
}

func runLifecycle(ctx context.Context, st WorkerStore, exec LifecycleExecutor, log *slog.Logger, taskID, vmID, nodeID uuid.UUID, op string, desiredPhase store.VMDesiredPhase, runtimePhase store.VMPhase, failureCode string) error {
	if err := st.UpdateTaskRunning(ctx, taskID); err != nil {
		return fmt.Errorf("update task running: %v", err)
	}
	vm, err := st.VMByID(ctx, vmID)
	if err != nil {
		return failRun(ctx, st, log, "vms."+op, taskID, classifyLoadErr(err, errCodeVMNotFound), fmt.Errorf("load vm: %v", err))
	}
	node, err := st.NodeByID(ctx, nodeID)
	if err != nil {
		return failRun(ctx, st, log, "vms."+op, taskID, classifyLoadErr(err, errCodeVMNodeMissing), fmt.Errorf("load node: %v", err))
	}
	task, err := st.TaskByID(ctx, taskID)
	if err != nil {
		return fmt.Errorf("reload task: %v", err)
	}

	result, execErr := exec.Execute(ctx, op, LifecycleArgs{
		TaskID:        taskID,
		AgentTaskID:   task.AgentTaskID,
		VM:            vm,
		Node:          node,
		OnAgentTaskID: onAgentTaskID(st, taskID),
	})
	if execErr != nil {
		return failRun(ctx, st, log, "vms."+op, taskID, classifyVMError(execErr, failureCode), execErr)
	}

	resultJSON, err := json.Marshal(result)
	if err != nil {
		return failRun(ctx, st, log, "vms."+op, taskID, "internal", fmt.Errorf("marshal lifecycle result: %v", err))
	}
	if err := st.ProjectVMLifecycleSuccess(ctx, vmID, desiredPhase, runtimePhase,
		store.UpdateTaskFinalizedParams{ID: taskID, Status: store.TaskStatusSuccess, Result: resultJSON},
	); err != nil {
		return fmt.Errorf("project lifecycle success: %v", err)
	}
	return nil
}

// onAgentTaskID returns the resumption callback the executor invokes after the
// agent's 202, persisting the agent task id through the task mutator.
func onAgentTaskID(st WorkerStore, taskID uuid.UUID) func(context.Context, uuid.UUID) error {
	return func(ctx context.Context, agentTaskID uuid.UUID) error {
		return st.UpdateTaskAgentTaskID(ctx, store.UpdateTaskAgentTaskIDParams{ID: taskID, AgentTaskID: &agentTaskID})
	}
}

// classifyLoadErr maps an entity-read error to a *_not_found code (when the row
// is absent) or "internal" otherwise. The etcd analogue of classifyLoadError
// (which keys on pgx.ErrNoRows).
func classifyLoadErr(err error, notFoundCode string) string {
	if errors.Is(err, store.ErrNotFound) {
		return notFoundCode
	}
	return "internal"
}

// failRun writes the terminal failed envelope and returns cause so the
// dispatcher decides requeue vs fail against the kind's attempt budget. Mirrors
// the river workers' fail() against the WorkerStore mutator.
func failRun(ctx context.Context, st WorkerStore, log *slog.Logger, op string, taskID uuid.UUID, code string, cause error) error {
	envelope, marshalErr := marshalTaskError(code, cause.Error())
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
