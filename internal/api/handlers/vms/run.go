// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/store"
)

// Run-form async VM workers for the etcd job runtime. These
// reuse the executor seams, Args payloads, and error classifiers of the former
// workers but drive the orchestration against the WorkerStore interface (the
// etcd store's task mutators + entity reads + atomic projections). The handler
// constructors return functions assignable to
// worker.Handler; cmd/api registers them on the dispatcher.

// WorkerStore is the storage surface the async VM worker handlers depend on:
// task-lifecycle mutators, entity reads, and the atomic success projections.
// *etcdstore.Store satisfies it (asserted in the etcdstore integration tests).
type WorkerStore interface {
	UpdateTaskRunning(ctx context.Context, id uuid.UUID) (alreadyTerminal bool, err error)
	UpdateTaskFinalized(ctx context.Context, arg store.UpdateTaskFinalizedParams) error
	UpdateTaskAgentTaskID(ctx context.Context, arg store.UpdateTaskAgentTaskIDParams) error
	ClearTaskAgentTaskID(ctx context.Context, id uuid.UUID) error
	TaskByID(ctx context.Context, id uuid.UUID) (store.Task, error)
	VMByID(ctx context.Context, id uuid.UUID) (store.VM, error)
	ListVMDisksByVM(ctx context.Context, vmID uuid.UUID) ([]store.VMDisk, error)
	ListVMNicsByVM(ctx context.Context, vmID uuid.UUID) ([]store.VMNic, error)
	NetworkByID(ctx context.Context, id uuid.UUID) (store.Network, error)
	StoragePoolByID(ctx context.Context, id uuid.UUID) (store.StoragePool, error)
	NodeByID(ctx context.Context, id uuid.UUID) (store.Node, error)
	ProjectVMCreateSuccess(ctx context.Context, rt store.UpsertVMRuntimeParams, fin store.UpdateTaskFinalizedParams, imageSHA256 []byte) error
	ProjectVMDeleteSuccess(ctx context.Context, vm store.VM, fin store.UpdateTaskFinalizedParams) error
	ProjectVMLifecycleSuccess(ctx context.Context, vmID uuid.UUID, desiredPhase store.VMDesiredPhase, runtimePhase store.VMPhase, fin store.UpdateTaskFinalizedParams) error
}

// defaultStaleGrace is the fallback heartbeat-staleness window used by the
// create/lifecycle/delete handlers when their staleGrace is unset (<= 0): past
// it an owning node is treated as terminally dead.
const defaultStaleGrace = 5 * time.Minute

// CreateHandler returns the dispatcher handler for vm.create jobs. The VM row is
// self-describing - it carries the image URL / digest / format - so the worker
// hands the image source straight to the agent (the agent does the
// IfNotPresent pull into the pool cache). staleGrace is the heartbeat-staleness
// window past which an owning node is treated as terminally dead (the create
// then fails fast and terminally rather than burning the retry budget); it
// defaults to 5 minutes when <= 0.
func CreateHandler(st WorkerStore, exec CreateExecutor, log *slog.Logger, staleGrace time.Duration) func(context.Context, []byte) error {
	if staleGrace <= 0 {
		staleGrace = defaultStaleGrace
	}
	return func(ctx context.Context, raw []byte) error {
		var args VMCreateArgs
		if err := json.Unmarshal(raw, &args); err != nil {
			return fmt.Errorf("unmarshal vm.create args: %v", err)
		}
		return runCreate(ctx, st, exec, log, args, staleGrace)
	}
}

func runCreate(ctx context.Context, st WorkerStore, exec CreateExecutor, log *slog.Logger, args VMCreateArgs, staleGrace time.Duration) error {
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
	vm, err := st.VMByID(ctx, args.VMID)
	if err != nil {
		return failRun(ctx, st, log, "vms.create", taskID, classifyLoadErr(err, errCodeVMNotFound), fmt.Errorf("load vm: %v", err))
	}
	disk, err := loadCreateDisk(ctx, st, log, taskID, args.VMID)
	if err != nil {
		return err
	}
	pool, err := st.StoragePoolByID(ctx, args.PoolID)
	if err != nil {
		return failRun(ctx, st, log, "vms.create", taskID, classifyLoadErr(err, errCodeVMPoolMissing), fmt.Errorf("load pool: %v", err))
	}
	node, nodeErr := st.NodeByID(ctx, args.NodeID)
	if nodeErr != nil && !errors.Is(nodeErr, store.ErrNotFound) {
		// Transient store error: retryable.
		return failRun(ctx, st, log, "vms.create", taskID, "internal", fmt.Errorf("load node: %v", nodeErr))
	}
	if nodeTerminallyDead(node, nodeErr, staleGrace) {
		// No agent will ever service a create on a gone/unreachable node; fail
		// terminally rather than burning the retry budget on a doomed op. This
		// does NOT project success - the VM was not created, so the task is
		// failed.
		return failTerminal(ctx, st, log, "vms.create", taskID, errCodeVMNodeMissing,
			fmt.Errorf("owning node %s is gone or unreachable; cannot create vm", args.NodeID))
	}
	nics, err := resolveCreateNICs(ctx, st, args.VMID)
	if err != nil {
		return failRun(ctx, st, log, "vms.create", taskID, "internal", err)
	}
	task, err := st.TaskByID(ctx, taskID)
	if err != nil {
		return fmt.Errorf("reload task: %v", err)
	}

	result, execErr := exec.Execute(ctx, CreateArgs{
		TaskID:        taskID,
		AgentTaskID:   task.AgentTaskID,
		VM:            vm,
		Disk:          disk,
		ImageURL:      vm.ImageURL,
		ImageSHA256:   hex.EncodeToString(vm.ImageSHA256),
		Format:        string(vm.ImageFormat),
		DiskGiB:       int(disk.SizeGib),
		Pool:          pool,
		Node:          node,
		NICs:          nics,
		OnAgentTaskID: onAgentTaskID(st, taskID),
	})
	if execErr != nil {
		return failCreateExec(ctx, st, log, taskID, execErr)
	}

	resultJSON, err := json.Marshal(result)
	if err != nil {
		return failRun(ctx, st, log, "vms.create", taskID, "internal", fmt.Errorf("marshal create result: %v", err))
	}
	nodeID := args.NodeID
	// Decode the agent-resolved hex digest so the projection can stamp it onto
	// the VM row. A malformed digest (never expected from a successful agent
	// task) degrades to no stamp rather than failing the committed create.
	resolvedDigest, decErr := hex.DecodeString(result.ImageSHA256)
	if decErr != nil {
		resolvedDigest = nil
	}
	if err := st.ProjectVMCreateSuccess(ctx,
		store.UpsertVMRuntimeParams{VmID: args.VMID, CurrentNodeID: &nodeID, Phase: store.VmPhaseRunning, ObservedGeneration: 1},
		store.UpdateTaskFinalizedParams{ID: taskID, Status: store.TaskStatusSuccess, Result: resultJSON},
		resolvedDigest,
	); err != nil {
		return fmt.Errorf("project create success: %v", err)
	}
	return nil
}

// loadCreateDisk loads the VM's disks and returns the boot disk (device order
// 0, the first row). A read error or a VM with no disks is an internal
// inconsistency (the disk row is created atomically with the VM): the task is
// finalized failed and the non-nil error is propagated for requeue.
func loadCreateDisk(ctx context.Context, st WorkerStore, log *slog.Logger, taskID, vmID uuid.UUID) (store.VMDisk, error) {
	disks, err := st.ListVMDisksByVM(ctx, vmID)
	if err != nil {
		return store.VMDisk{}, failRun(ctx, st, log, "vms.create", taskID, "internal", fmt.Errorf("list vm disks: %v", err))
	}
	if len(disks) == 0 {
		return store.VMDisk{}, failRun(ctx, st, log, "vms.create", taskID, "internal", fmt.Errorf("vm %s has no disks", vmID))
	}
	return disks[0], nil
}

// resolveCreateNICs loads the VM's vm_nics and resolves each against its
// network, producing the fully-resolved agent-facing NIC list: bridge name +
// MTU come from the network, MAC / model / device order from the vm_nic. An
// empty result means the VM has no NICs (the agent falls back to SLIRP). A
// missing network is an internal inconsistency (the row was created atomically
// with the NIC) and fails the task.
func resolveCreateNICs(ctx context.Context, st WorkerStore, vmID uuid.UUID) ([]agentclient.VMCreateNIC, error) {
	rows, err := st.ListVMNicsByVM(ctx, vmID)
	if err != nil {
		return nil, fmt.Errorf("list vm nics: %v", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]agentclient.VMCreateNIC, 0, len(rows))
	for _, n := range rows {
		net, err := st.NetworkByID(ctx, n.NetworkID)
		if err != nil {
			return nil, fmt.Errorf("resolve network %s for nic %s: %v", n.NetworkID, n.ID, err)
		}
		out = append(out, agentclient.VMCreateNIC{
			ID:          n.ID,
			Bridge:      net.BridgeName,
			MAC:         n.MacAddress.String(),
			Model:       string(n.Model),
			MTU:         int(net.Mtu),
			DeviceOrder: int(n.DeviceOrder),
		})
	}
	return out, nil
}

// DeleteHandler returns the dispatcher handler for vm.delete jobs. staleGrace is
// the heartbeat-staleness window past which an owning node is treated as
// terminally dead regardless of status; it defaults to 5 minutes when <= 0.
func DeleteHandler(st WorkerStore, exec DeleteExecutor, log *slog.Logger, staleGrace time.Duration) func(context.Context, []byte) error {
	if staleGrace <= 0 {
		staleGrace = defaultStaleGrace
	}
	return func(ctx context.Context, raw []byte) error {
		var args VMDeleteArgs
		if err := json.Unmarshal(raw, &args); err != nil {
			return fmt.Errorf("unmarshal vm.delete args: %v", err)
		}
		return runDelete(ctx, st, exec, log, staleGrace, args)
	}
}

// nodeTerminallyDead reports whether the VM's owning node is beyond any agent
// teardown, so the delete must be projected directly rather than dispatched to
// an agent. True when the node row is gone or force-deleted (ErrNotFound), or
// when the node has been unseen for longer than staleGrace.
//
// 'unreachable' with a fresh heartbeat is NOT terminal - the node may merely be
// partitioned with qemu still running, and the agent's VM reconciler does not
// prune VMs the CP stops declaring (it reports them and waits), so skipping the
// agent for a node that later heals would leak that qemu forever. Such a node
// gets a best-effort agent teardown first, falling back to a direct projection
// only when that fails.
//
// The staleness arm covers a cordoned/draining node that dies: it never
// advances to 'gone' (the reaper only flips ready/pending), so without it the
// VM delete would failRun forever. Its worst case is skipping the agent for a
// node that healed within the window, leaking one qemu (the accepted leak
// scale) - it keys on a durable timestamp, not inferred state.
func nodeTerminallyDead(node store.Node, nodeErr error, staleGrace time.Duration) bool {
	if errors.Is(nodeErr, store.ErrNotFound) || node.Status == store.NodeStatusGone {
		return true
	}
	return node.LastHeartbeatAt == nil || node.LastHeartbeatAt.Before(time.Now().Add(-staleGrace))
}

func runDelete(ctx context.Context, st WorkerStore, exec DeleteExecutor, log *slog.Logger, staleGrace time.Duration, args VMDeleteArgs) error {
	taskID := args.TaskID
	alreadyTerminal, err := st.UpdateTaskRunning(ctx, taskID)
	if err != nil {
		return fmt.Errorf("update task running: %v", err)
	}
	if alreadyTerminal {
		// The task already committed success/cancelled: do NOT contact the agent.
		// Return nil so the dispatcher CompleteJob-deletes the job.
		return nil
	}
	vm, err := st.VMByID(ctx, args.VMID)
	if err != nil {
		return failRun(ctx, st, log, "vms.delete", taskID, classifyLoadErr(err, errCodeVMNotFound), fmt.Errorf("load vm: %v", err))
	}
	node, nodeErr := st.NodeByID(ctx, args.NodeID)
	if nodeErr != nil && !errors.Is(nodeErr, store.ErrNotFound) {
		return failRun(ctx, st, log, "vms.delete", taskID, "internal", fmt.Errorf("load node: %v", nodeErr))
	}
	if !nodeTerminallyDead(node, nodeErr, staleGrace) {
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
			if cerr := clearAgentTaskIDIfGone(ctx, st, taskID, execErr); cerr != nil {
				return cerr
			}
			if node.Status != store.NodeStatusUnreachable {
				return failRun(ctx, st, log, "vms.delete", taskID, classifyVMError(execErr, errCodeVMDeleteFailed), execErr)
			}
			// Unreachable node, agent teardown failed: the node is genuinely
			// down. Fall through to the direct projection below so cleanup is
			// not blocked on a node that cannot answer.
			log.WarnContext(ctx, "agent teardown failed for unreachable node; projecting delete directly",
				slog.String("vm_id", args.VMID.String()), slog.String("node_id", args.NodeID.String()),
				slog.String("err", execErr.Error()))
		} else {
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
	}

	resultJSON, merr := json.Marshal(struct {
		VMID         string `json:"vm_id"`
		SkippedAgent bool   `json:"skipped_agent"`
		Reason       string `json:"reason"`
	}{VMID: args.VMID.String(), SkippedAgent: true, Reason: "owning node not serviceable"})
	if merr != nil {
		return failRun(ctx, st, log, "vms.delete", taskID, "internal", fmt.Errorf("marshal delete result: %v", merr))
	}
	if err := st.ProjectVMDeleteSuccess(ctx, vm,
		store.UpdateTaskFinalizedParams{ID: taskID, Status: store.TaskStatusSuccess, Result: resultJSON},
	); err != nil {
		return fmt.Errorf("project delete success: %v", err)
	}
	log.InfoContext(ctx, "vm deleted without agent teardown (owning node not serviceable)",
		slog.String("vm_id", args.VMID.String()), slog.String("node_id", args.NodeID.String()))
	return nil
}

// LifecycleHandler returns the dispatcher handler for one of the async lifecycle
// kinds (vm.start / vm.stop / vm.poweroff / vm.reboot). op is the agent-side
// action segment; desiredPhase is written on success (empty = unchanged, e.g.
// reboot); runtimePhase is the observed phase projected through.
// staleGrace is the heartbeat-staleness window past which an owning node is
// treated as terminally dead (the op then fails fast and terminally rather than
// burning the retry budget); it defaults to 5 minutes when <= 0.
func LifecycleHandler(st WorkerStore, exec LifecycleExecutor, log *slog.Logger, op string, desiredPhase store.VMDesiredPhase, runtimePhase store.VMPhase, failureCode string, staleGrace time.Duration) func(context.Context, []byte) error {
	if staleGrace <= 0 {
		staleGrace = defaultStaleGrace
	}
	return func(ctx context.Context, raw []byte) error {
		var args VMStartArgs // every lifecycle Args shares {task_id, vm_id, node_id}
		if err := json.Unmarshal(raw, &args); err != nil {
			return fmt.Errorf("unmarshal vm.%s args: %v", op, err)
		}
		return runLifecycle(ctx, st, exec, log, args.TaskID, args.VMID, args.NodeID, op, desiredPhase, runtimePhase, failureCode, staleGrace)
	}
}

func runLifecycle(ctx context.Context, st WorkerStore, exec LifecycleExecutor, log *slog.Logger, taskID, vmID, nodeID uuid.UUID, op string, desiredPhase store.VMDesiredPhase, runtimePhase store.VMPhase, failureCode string, staleGrace time.Duration) error {
	alreadyTerminal, err := st.UpdateTaskRunning(ctx, taskID)
	if err != nil {
		return fmt.Errorf("update task running: %v", err)
	}
	if alreadyTerminal {
		// The task already committed success/cancelled: do NOT contact the agent.
		// Return nil so the dispatcher CompleteJob-deletes the job.
		return nil
	}
	vm, err := st.VMByID(ctx, vmID)
	if err != nil {
		return failRun(ctx, st, log, "vms."+op, taskID, classifyLoadErr(err, errCodeVMNotFound), fmt.Errorf("load vm: %v", err))
	}
	node, nodeErr := st.NodeByID(ctx, nodeID)
	if nodeErr != nil && !errors.Is(nodeErr, store.ErrNotFound) {
		// Transient store error: retryable.
		return failRun(ctx, st, log, "vms."+op, taskID, "internal", fmt.Errorf("load node: %v", nodeErr))
	}
	if nodeTerminallyDead(node, nodeErr, staleGrace) {
		// No agent will ever service a lifecycle op on a gone/unreachable node;
		// fail terminally rather than burning the retry budget on a doomed op.
		// This does NOT project success - the op did not happen, so projecting
		// success would lie about runtime state.
		return failTerminal(ctx, st, log, "vms."+op, taskID, errCodeVMNodeMissing,
			fmt.Errorf("owning node %s is gone or unreachable; cannot %s vm", nodeID, op))
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

// LifecycleKind describes one async lifecycle job kind and the success/failure
// projection parameters LifecycleHandler needs. It exists so cmd/api can
// register the four lifecycle handlers on the etcd dispatcher without reaching
// into this package's unexported failure-code constants.
type LifecycleKind struct {
	Kind         string               // queue job kind ("vm.start", ...)
	Op           string               // agent-side action segment passed to the executor
	DesiredPhase store.VMDesiredPhase // vms.desired_phase written on success ("" = unchanged)
	RuntimePhase store.VMPhase        // vm_runtime.phase written on success
	FailureCode  string               // terminal failure envelope code
}

// LifecycleKinds returns the four async lifecycle kinds with their projection
// parameters, matching the river LifecycleWorkers registration verbatim:
// start → running/running, stop and poweroff → stopped/stopped, reboot leaves
// the desired phase unchanged (the runtime cycles, the user intent does not)
// and observes running.
func LifecycleKinds() []LifecycleKind {
	return []LifecycleKind{
		{Kind: "vm.start", Op: "start", DesiredPhase: store.VmDesiredPhaseRunning, RuntimePhase: store.VmPhaseRunning, FailureCode: errCodeVMStartFailed},
		{Kind: "vm.stop", Op: "stop", DesiredPhase: store.VmDesiredPhaseStopped, RuntimePhase: store.VmPhaseStopped, FailureCode: errCodeVMStopFailed},
		{Kind: "vm.poweroff", Op: "poweroff", DesiredPhase: store.VmDesiredPhaseStopped, RuntimePhase: store.VmPhaseStopped, FailureCode: errCodeVMPoweroffFailed},
		{Kind: "vm.reboot", Op: "reboot", DesiredPhase: store.VMDesiredPhase(""), RuntimePhase: store.VmPhaseRunning, FailureCode: errCodeVMRebootFailed},
	}
}

// onAgentTaskID returns the resumption callback the executor invokes after the
// agent's 202, persisting the agent task id through the task mutator.
func onAgentTaskID(st WorkerStore, taskID uuid.UUID) func(context.Context, uuid.UUID) error {
	return func(ctx context.Context, agentTaskID uuid.UUID) error {
		return st.UpdateTaskAgentTaskID(ctx, store.UpdateTaskAgentTaskIDParams{ID: taskID, AgentTaskID: &agentTaskID})
	}
}

// isAgentTaskGone reports whether err carries an agent 404 - the agent task
// vanished (typically the agent restarted and lost its in-memory task store), so
// the persisted agent_task_id is stale and the redelivery should re-POST.
func isAgentTaskGone(err error) bool {
	var ae *agentclient.AgentError
	return errors.As(err, &ae) && ae.Status == 404
}

// failCreateExec handles a vm.create executor error: it clears a stale
// agent_task_id on a permanent agent-task 404 (so the redelivery re-POSTs and the
// agent's durable dedup reconciles, R2-M2) and then finalizes the task failed
// with retry semantics. A clear-write failure preempts and is returned so the
// caller requeues.
func failCreateExec(ctx context.Context, st WorkerStore, log *slog.Logger, taskID uuid.UUID, execErr error) error {
	if cerr := clearAgentTaskIDIfGone(ctx, st, taskID, execErr); cerr != nil {
		return cerr
	}
	return failRun(ctx, st, log, "vms.create", taskID, classifyVMError(execErr, errCodeVMCreateFailed), execErr)
}

// clearAgentTaskIDIfGone clears the task's persisted agent_task_id when execErr
// is a permanent agent-task 404 (the agent task vanished, typically after an
// agent restart), so the redelivery re-POSTs and the agent's durable dedup
// (m.vms) reconciles instead of re-polling a vanished task forever (R2-M2). It is
// a no-op for any other error. A clear-write failure is returned so the caller
// requeues; otherwise nil and the caller proceeds to failRun. Safe: the re-POST
// cannot clobber a live VM (R2-M1 / ADR 0042).
func clearAgentTaskIDIfGone(ctx context.Context, st WorkerStore, taskID uuid.UUID, execErr error) error {
	if !isAgentTaskGone(execErr) {
		return nil
	}
	if err := st.ClearTaskAgentTaskID(ctx, taskID); err != nil {
		return fmt.Errorf("clear agent_task_id: %v", err)
	}
	return nil
}

// classifyLoadErr maps an entity-read error to a *_not_found code (when the row
// is absent) or "internal" otherwise. Keys on store.ErrNotFound.
func classifyLoadErr(err error, notFoundCode string) string {
	if errors.Is(err, store.ErrNotFound) {
		return notFoundCode
	}
	return "internal"
}

// finalizeFailed writes the terminal failed envelope for taskID. It returns nil
// on a successful write, or a wrapped error if the finalize write itself failed
// (the caller should retry so the envelope eventually persists). Mirrors the
// river workers' fail() against the WorkerStore mutator.
func finalizeFailed(ctx context.Context, st WorkerStore, log *slog.Logger, op string, taskID uuid.UUID, code string, cause error) error {
	envelope, marshalErr := marshalTaskError(code, cause.Error())
	if marshalErr != nil {
		envelope = []byte(`{"code":"internal","message":"marshal error envelope failed"}`)
		log.ErrorContext(ctx, op+" marshal error envelope failed", "task_id", taskID, "code", code, "error", marshalErr)
	}
	if err := st.UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{ID: taskID, Status: store.TaskStatusFailed, Error: envelope}); err != nil {
		log.ErrorContext(ctx, op+" finalize-failed write failed", "task_id", taskID, "code", code, "error", err)
		return fmt.Errorf("finalize failed: %v (cause: %v)", err, cause)
	}
	return nil
}

// failRun writes the failed envelope and returns cause, so the dispatcher
// RETRIES (the failure may be transient) against the kind's attempt budget. A
// finalize-write error preempts cause and is returned so the envelope persists.
func failRun(ctx context.Context, st WorkerStore, log *slog.Logger, op string, taskID uuid.UUID, code string, cause error) error {
	if err := finalizeFailed(ctx, st, log, op, taskID, code, cause); err != nil {
		return err
	}
	return cause
}

// failTerminal writes the failed envelope and returns nil, so the dispatcher
// COMPLETES (deletes) the job instead of retrying. Use it for conditions that
// cannot become satisfiable on retry (the owning node is gone/unreachable),
// avoiding a doomed op burning its whole attempt budget. A finalize-write error
// is still returned (retry so the envelope persists).
func failTerminal(ctx context.Context, st WorkerStore, log *slog.Logger, op string, taskID uuid.UUID, code string, cause error) error {
	return finalizeFailed(ctx, st, log, op, taskID, code, cause)
}
