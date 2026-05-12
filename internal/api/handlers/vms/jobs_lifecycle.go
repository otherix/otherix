// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/otherix/otherix/internal/store"
)

// VMStartArgs / VMStopArgs / VMPoweroffArgs / VMRebootArgs are the
// river JobArgs for the L2 async lifecycle pipeline. The atomic-
// enqueue handlers insert the task row, mint а fresh task id, и
// enqueue this payload via riverClient.InsertTx. The worker re-loads
// the VM + node from the database before dispatching к the executor —
// same pattern as VMCreateArgs / VMDeleteArgs.
type VMStartArgs struct {
	TaskID uuid.UUID `json:"task_id"`
	VMID   uuid.UUID `json:"vm_id"`
	NodeID uuid.UUID `json:"node_id"`
}

// Kind names the job kind.
func (VMStartArgs) Kind() string { return "vm.start" }

// VMStopArgs is the river JobArgs for а `vm.stop` task.
type VMStopArgs struct {
	TaskID uuid.UUID `json:"task_id"`
	VMID   uuid.UUID `json:"vm_id"`
	NodeID uuid.UUID `json:"node_id"`
}

// Kind names the job kind.
func (VMStopArgs) Kind() string { return "vm.stop" }

// VMPoweroffArgs is the river JobArgs for а `vm.poweroff` task.
type VMPoweroffArgs struct {
	TaskID uuid.UUID `json:"task_id"`
	VMID   uuid.UUID `json:"vm_id"`
	NodeID uuid.UUID `json:"node_id"`
}

// Kind names the job kind.
func (VMPoweroffArgs) Kind() string { return "vm.poweroff" }

// VMRebootArgs is the river JobArgs for а `vm.reboot` task.
type VMRebootArgs struct {
	TaskID uuid.UUID `json:"task_id"`
	VMID   uuid.UUID `json:"vm_id"`
	NodeID uuid.UUID `json:"node_id"`
}

// Kind names the job kind.
func (VMRebootArgs) Kind() string { return "vm.reboot" }

// LifecycleArgs is the per-task input the async lifecycle executors
// receive. Shared by all four L2 operations because the agent-side
// dispatch shape (endpoint, vmName, idempotency key) is identical.
// AgentTaskID carries the persisted agent-side task uuid for
// resumption — nil on first run, non-nil when the worker is recovering
// from а CP-side restart.
//
// OnAgentTaskID is the callback the executor invokes immediately
// after the agent's 202 returns, persisting the agent task id via
// UpdateTaskAgentTaskID. Same shape as CreateArgs / DeleteArgs.
type LifecycleArgs struct {
	TaskID        uuid.UUID
	AgentTaskID   *uuid.UUID
	VM            store.Vm
	Node          store.Node
	OnAgentTaskID func(ctx context.Context, agentTaskID uuid.UUID) error
}

// LifecycleResult is the executor's output. Surfaces verbatim into
// the task's `result` JSONB so operators can correlate без а follow-
// up GET.
type LifecycleResult struct {
	VMID string `json:"vm_id"`
}

// LifecycleExecutor is the per-task-type seam for vm.start /
// vm.stop / vm.poweroff / vm.reboot. Production implementation lives
// in agent_executors_lifecycle.go; tests pass а stub. The op string
// is one of "start" / "stop" / "poweroff" / "reboot" — drives the
// agent-side endpoint selection.
type LifecycleExecutor interface {
	Execute(ctx context.Context, op string, args LifecycleArgs) (LifecycleResult, error)
}

// LifecycleDepsAsync is the dependency bundle BuildRiverClient passes
// to the four lifecycle worker registrations. Executor optional —
// nil-tolerant в the same way as CreateDeps / DeleteDeps. Named
// LifecycleDepsAsync rather than LifecycleDeps к avoid colliding с
// the sync-lifecycle LifecycleDeps в lifecycle.go.
type LifecycleDepsAsync struct {
	Store    *store.Store
	Executor LifecycleExecutor
	Logger   *slog.Logger
}

// vmLifecycleAsyncWorker is the shared worker engine that drives the
// four async lifecycle operations. The four kind-specific worker
// types are thin shells над this engine; they differ only в the
// TaskKind / Args они unmarshal и the op string they pass through к
// the executor + desired_phase write.
//
// Sequence: UpdateTaskRunning → load vm + node → reload task для
// AgentTaskID → executor.Execute → on success write vms.desired_phase
// (when applicable) + UpdateTaskFinalized в InTx → on error
// classifyVMError + fail. Mirrors the VMCreateWorker / VMDeleteWorker
// shape verbatim except for the dispatch indirection.
type vmLifecycleAsyncWorker struct {
	store    *store.Store
	executor LifecycleExecutor
	logger   *slog.Logger
}

// Failure codes specific к the L2 async surface. Pass-through agent
// codes (stop_timeout, qemu_supervision_failed, qmp_unavailable, ...)
// are preserved verbatim by classifyVMError.
const (
	errCodeVMStartFailed    = "vm_start_failed"
	errCodeVMStopFailed     = "vm_stop_failed"
	errCodeVMPoweroffFailed = "vm_poweroff_failed"
	errCodeVMRebootFailed   = "vm_reboot_failed"
)

// runOne dispatches one async lifecycle job. Used by each of the
// four kind-specific worker wrappers. op is the agent-side action
// segment ("start" / "stop" / ...). desiredPhase is the
// vms.desired_phase к write on success per CP spec: start → running,
// stop / poweroff → stopped, reboot → unchanged (caller passes
// VmDesiredPhase("")). runtimePhase is the vm_runtime.phase к write
// on success — mirrors the agent's observed outcome verbatim so the
// projected `status` field converges immediately rather than waiting
// for а heartbeat-driven reconcile. L3 reconciler eventually owns
// vm_runtime.phase; until then the worker writes through.
func (w *vmLifecycleAsyncWorker) runOne(
	ctx context.Context,
	taskID, vmID, nodeID uuid.UUID,
	op string,
	desiredPhase store.VmDesiredPhase,
	runtimePhase store.VmPhase,
	failureCode string,
) error {
	if w.executor == nil {
		return fmt.Errorf("vm.%s executor not configured (Workers.Enabled likely false at boot)", op)
	}

	if err := w.store.Queries().UpdateTaskRunning(ctx, taskID); err != nil {
		return fmt.Errorf("update task running: %v", err)
	}

	vm, err := w.store.Queries().GetVMByID(ctx, vmID)
	if err != nil {
		return failTask(ctx, w.store, w.logger, "vms."+op, taskID,
			classifyLoadError(err, errCodeVMNotFound), fmt.Errorf("load vm: %v", err))
	}
	node, err := w.store.Queries().GetNodeByID(ctx, nodeID)
	if err != nil {
		return failTask(ctx, w.store, w.logger, "vms."+op, taskID,
			classifyLoadError(err, errCodeVMNodeMissing), fmt.Errorf("load node: %v", err))
	}

	task, err := w.store.Queries().GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("reload task: %v", err)
	}

	result, execErr := w.executor.Execute(ctx, op, LifecycleArgs{
		TaskID:        taskID,
		AgentTaskID:   task.AgentTaskID,
		VM:            vm,
		Node:          node,
		OnAgentTaskID: persistAgentTaskID(w.store, taskID),
	})
	if execErr != nil {
		return failTask(ctx, w.store, w.logger, "vms."+op, taskID,
			classifyVMError(execErr, failureCode), execErr)
	}

	return w.projectSuccess(ctx, taskID, vmID, desiredPhase, runtimePhase, result)
}

// projectSuccess writes vms.desired_phase (when non-empty),
// vm_runtime.phase, и finalises the task in one transaction.
// Bundling всех three writes под one InTx keeps the projected
// `status` field consistent с the runtime — а half-applied
// terminal would surface а VM where the user's desire converged but
// the observed runtime stayed stale until the next heartbeat.
func (w *vmLifecycleAsyncWorker) projectSuccess(
	ctx context.Context,
	taskID, vmID uuid.UUID,
	desiredPhase store.VmDesiredPhase,
	runtimePhase store.VmPhase,
	result LifecycleResult,
) error {
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return failTask(ctx, w.store, w.logger, "vms.lifecycle", taskID,
			"internal", fmt.Errorf("marshal lifecycle result: %v", err))
	}
	return w.store.InTx(ctx, func(q *store.Queries) error {
		if desiredPhase != "" {
			if err := q.UpdateVMDesiredPhase(ctx, store.UpdateVMDesiredPhaseParams{
				ID:           vmID,
				DesiredPhase: desiredPhase,
			}); err != nil {
				return fmt.Errorf("update desired_phase: %v", err)
			}
		}
		if err := q.UpdateVMRuntimePhase(ctx, store.UpdateVMRuntimePhaseParams{
			VmID:  vmID,
			Phase: runtimePhase,
		}); err != nil {
			return fmt.Errorf("update vm_runtime phase: %v", err)
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

// VMStartWorker drives the vm.start lifecycle.
type VMStartWorker struct {
	river.WorkerDefaults[VMStartArgs]
	engine *vmLifecycleAsyncWorker
}

// Work executes one VMStartArgs job: dispatches к the shared engine
// с op="start", desiredPhase="running", runtimePhase="running".
func (w *VMStartWorker) Work(ctx context.Context, j *river.Job[VMStartArgs]) error {
	return w.engine.runOne(ctx,
		j.Args.TaskID, j.Args.VMID, j.Args.NodeID,
		"start", store.VmDesiredPhaseRunning, store.VmPhaseRunning, errCodeVMStartFailed)
}

// VMStopWorker drives the vm.stop lifecycle.
type VMStopWorker struct {
	river.WorkerDefaults[VMStopArgs]
	engine *vmLifecycleAsyncWorker
}

// Work executes one VMStopArgs job: dispatches с op="stop",
// desiredPhase="stopped", runtimePhase="stopped".
func (w *VMStopWorker) Work(ctx context.Context, j *river.Job[VMStopArgs]) error {
	return w.engine.runOne(ctx,
		j.Args.TaskID, j.Args.VMID, j.Args.NodeID,
		"stop", store.VmDesiredPhaseStopped, store.VmPhaseStopped, errCodeVMStopFailed)
}

// VMPoweroffWorker drives the vm.poweroff lifecycle.
type VMPoweroffWorker struct {
	river.WorkerDefaults[VMPoweroffArgs]
	engine *vmLifecycleAsyncWorker
}

// Work executes one VMPoweroffArgs job: op="poweroff",
// desiredPhase="stopped", runtimePhase="stopped".
func (w *VMPoweroffWorker) Work(ctx context.Context, j *river.Job[VMPoweroffArgs]) error {
	return w.engine.runOne(ctx,
		j.Args.TaskID, j.Args.VMID, j.Args.NodeID,
		"poweroff", store.VmDesiredPhaseStopped, store.VmPhaseStopped, errCodeVMPoweroffFailed)
}

// VMRebootWorker drives the vm.reboot lifecycle.
type VMRebootWorker struct {
	river.WorkerDefaults[VMRebootArgs]
	engine *vmLifecycleAsyncWorker
}

// Work executes one VMRebootArgs job: op="reboot", desiredPhase
// unchanged (reboot leaves the user intent at "running" per CP spec
// — the runtime cycles, the desire does not), runtimePhase="running"
// (the post-cycle observed state is а fresh QEMU process в running).
func (w *VMRebootWorker) Work(ctx context.Context, j *river.Job[VMRebootArgs]) error {
	return w.engine.runOne(ctx,
		j.Args.TaskID, j.Args.VMID, j.Args.NodeID,
		"reboot", store.VmDesiredPhase(""), store.VmPhaseRunning, errCodeVMRebootFailed)
}

// LifecycleWorkers registers the four async lifecycle workers on the
// bundle. Same registration shape as Workers (create + delete); a
// nil executor is tolerated so cmd/api/main.go can wire workers
// without agent machinery when Workers.Enabled=false. Pgx pool +
// store + logger come from deps.
func LifecycleWorkers(workers *river.Workers, deps LifecycleDepsAsync) {
	engine := &vmLifecycleAsyncWorker{
		store:    deps.Store,
		executor: deps.Executor,
		logger:   deps.Logger,
	}
	river.AddWorker(workers, &VMStartWorker{engine: engine})
	river.AddWorker(workers, &VMStopWorker{engine: engine})
	river.AddWorker(workers, &VMPoweroffWorker{engine: engine})
	river.AddWorker(workers, &VMRebootWorker{engine: engine})
}
