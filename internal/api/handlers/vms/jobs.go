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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/store"
)

// VMCreateArgs is the river JobArgs for a `vm.create` task. The
// atomic-enqueue handler (Create above) inserts the task row, mints
// a fresh task id, and enqueues this payload via riverClient.InsertTx;
// the worker resolves vm / template / pool / node from the database
// before dispatching to the executor.
type VMCreateArgs struct {
	TaskID     uuid.UUID `json:"task_id"`
	VMID       uuid.UUID `json:"vm_id"`
	TemplateID uuid.UUID `json:"template_id"`
	PoolID     uuid.UUID `json:"pool_id"`
	NodeID     uuid.UUID `json:"node_id"`
}

// Kind names the job kind. Mirrors the OpenAPI Task.type value
// surfaced through tasks.{list,get}.
func (VMCreateArgs) Kind() string { return "vm.create" }

// VMDeleteArgs is the river JobArgs for a `vm.delete` task.
type VMDeleteArgs struct {
	TaskID uuid.UUID `json:"task_id"`
	VMID   uuid.UUID `json:"vm_id"`
	NodeID uuid.UUID `json:"node_id"`
}

// Kind names the job kind.
func (VMDeleteArgs) Kind() string { return "vm.delete" }

// CreateArgs is the per-task input the create executor receives.
// AgentTaskID carries the persisted agent-side task uuid for
// resumption - nil on first run, non-nil when the worker is
// recovering from a CP-side restart.
//
// OnAgentTaskID is the callback the executor invokes immediately
// after the agent's 202 returns, persisting the agent task id via
// UpdateTaskAgentTaskID. Same shape as the storage_image.import
// pattern — the executor never touches *store.Store directly.
type CreateArgs struct {
	TaskID        uuid.UUID
	AgentTaskID   *uuid.UUID
	VM            store.VM
	Disk          store.VMDisk
	Template      store.Template
	Pool          store.StoragePool
	Node          store.Node
	OnAgentTaskID func(ctx context.Context, agentTaskID uuid.UUID) error
}

// CreateResult is the create executor's output. agent_vm_id mirrors
// the supplied vm uuid (the agent uses the CP-minted id verbatim);
// surfaced into the task's `result` JSONB so operators can correlate
// without a follow-up GET.
type CreateResult struct {
	VMID string `json:"vm_id"`
}

// CreateExecutor is the per-task-type seam for vm.create. Production
// implementation is agentVMCreateExecutor over the agentclient
// package; tests use StubVMCreateExecutor.
type CreateExecutor interface {
	Execute(ctx context.Context, args CreateArgs) (CreateResult, error)
}

// DeleteArgs / DeleteResult / DeleteExecutor mirror the create seam
// for the vm.delete pipeline. Per Pre-L1 Path D the agent's
// `DELETE /v1/vms/{vm_name}` is name-keyed; the worker loads the
// VM row before dispatch so both the CP-side UUID (for the result
// projection) and the agent-facing name are available.
type DeleteArgs struct {
	TaskID        uuid.UUID
	AgentTaskID   *uuid.UUID
	VMID          uuid.UUID
	VMName        string
	Node          store.Node
	OnAgentTaskID func(ctx context.Context, agentTaskID uuid.UUID) error
}

// DeleteResult is the delete executor's output. Surfaces verbatim
// into the task's `result` JSONB so operators can correlate without
// a follow-up GET.
type DeleteResult struct {
	VMID string `json:"vm_id"`
}

// DeleteExecutor is the per-task-type seam for vm.delete. Production
// implementation is agentVMDeleteExecutor; tests use
// StubVMDeleteExecutor.
type DeleteExecutor interface {
	Execute(ctx context.Context, args DeleteArgs) (DeleteResult, error)
}

// CreateDeps is the dependency bundle BuildRiverClient passes to the
// vm.create worker registration. Executor optional: when nil, the
// worker is still registered (river requires it to accept Insert) but
// fails fast if Work is dispatched.
type CreateDeps struct {
	Store    *store.Store
	Executor CreateExecutor
	Logger   *slog.Logger
}

// DeleteDeps is the dependency bundle for vm.delete worker
// registration.
type DeleteDeps struct {
	Store    *store.Store
	Executor DeleteExecutor
	Logger   *slog.Logger
}

// Failure code constants for the task `error` envelope. Pass-through
// agent codes (`qemu_spawn_failed`, `template_not_found`,
// `pool_full`, …) are preserved verbatim by classifyVMError.
const (
	errCodeVMNotFound        = "vm_not_found"
	errCodeVMTemplateMissing = "template_not_found"
	errCodeVMPoolMissing     = "pool_not_found"
	errCodeVMNodeMissing     = "node_not_found"
	errCodeVMAgentUnreachabl = "agent_unreachable"
	errCodeVMTimeout         = "request_timeout"
	errCodeVMCreateFailed    = "vm_create_failed"
	errCodeVMDeleteFailed    = "vm_delete_failed"
)

// VMCreateWorker drives the vm.create lifecycle. Mirrors the
// StorageImageImportWorker pattern: pending → running → terminal,
// executor delegate, project + finalize in InTx.
type VMCreateWorker struct {
	river.WorkerDefaults[VMCreateArgs]
	store    *store.Store
	executor CreateExecutor
	logger   *slog.Logger
}

// Work executes one VMCreateArgs job:
//
//  1. UpdateTaskRunning.
//  2. Resolve vm / template / pool / node / disk; failures classify
//     to the matching `*_not_found` code.
//  3. Re-load task → AgentTaskID for resumption.
//  4. Delegate to executor with OnAgentTaskID callback.
//  5. On success: UpsertVMRuntime(running) + IncrementTemplateDerivedVMCount
//     + UpdateTaskFinalized in one InTx.
//  6. On error: classifyVMError + fail.
func (w *VMCreateWorker) Work(ctx context.Context, j *river.Job[VMCreateArgs]) error {
	if w.executor == nil {
		return fmt.Errorf("vm.create executor not configured (Workers.Enabled likely false at boot)")
	}

	taskID := j.Args.TaskID

	if err := w.store.Queries().UpdateTaskRunning(ctx, taskID); err != nil {
		return fmt.Errorf("update task running: %v", err)
	}

	vm, disk, tpl, pool, node, err := w.loadCreateEntities(ctx, taskID, j.Args)
	if err != nil {
		return err
	}

	task, err := w.store.Queries().GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("reload task: %v", err)
	}

	result, execErr := w.executor.Execute(ctx, CreateArgs{
		TaskID:        taskID,
		AgentTaskID:   task.AgentTaskID,
		VM:            vm,
		Disk:          disk,
		Template:      tpl,
		Pool:          pool,
		Node:          node,
		OnAgentTaskID: w.persistAgentTaskID(taskID),
	})
	if execErr != nil {
		return w.fail(ctx, taskID, classifyVMError(execErr, errCodeVMCreateFailed), execErr)
	}

	return w.projectCreateSuccess(ctx, taskID, j.Args, result)
}

// loadCreateEntities resolves the five FK targets needed by the
// executor. Each lookup classifies its own pgx.ErrNoRows to the
// matching error code.
func (w *VMCreateWorker) loadCreateEntities(
	ctx context.Context, taskID uuid.UUID, args VMCreateArgs,
) (store.VM, store.VMDisk, store.Template, store.StoragePool, store.Node, error) {
	vm, err := w.store.Queries().GetVMByID(ctx, args.VMID)
	if err != nil {
		return store.VM{}, store.VMDisk{}, store.Template{}, store.StoragePool{}, store.Node{},
			w.fail(ctx, taskID, classifyLoadError(err, errCodeVMNotFound), fmt.Errorf("load vm: %v", err))
	}
	disks, err := w.store.Queries().ListVMDisksByVM(ctx, args.VMID)
	if err != nil {
		return store.VM{}, store.VMDisk{}, store.Template{}, store.StoragePool{}, store.Node{},
			w.fail(ctx, taskID, "internal", fmt.Errorf("list vm disks: %v", err))
	}
	if len(disks) == 0 {
		return store.VM{}, store.VMDisk{}, store.Template{}, store.StoragePool{}, store.Node{},
			w.fail(ctx, taskID, "internal", fmt.Errorf("vm %s has no disks", args.VMID))
	}
	tpl, err := w.store.Queries().GetTemplate(ctx, args.TemplateID)
	if err != nil {
		return store.VM{}, store.VMDisk{}, store.Template{}, store.StoragePool{}, store.Node{},
			w.fail(ctx, taskID, classifyLoadError(err, errCodeVMTemplateMissing), fmt.Errorf("load template: %v", err))
	}
	pool, err := w.store.Queries().GetStoragePoolByID(ctx, args.PoolID)
	if err != nil {
		return store.VM{}, store.VMDisk{}, store.Template{}, store.StoragePool{}, store.Node{},
			w.fail(ctx, taskID, classifyLoadError(err, errCodeVMPoolMissing), fmt.Errorf("load pool: %v", err))
	}
	node, err := w.store.Queries().GetNodeByID(ctx, args.NodeID)
	if err != nil {
		return store.VM{}, store.VMDisk{}, store.Template{}, store.StoragePool{}, store.Node{},
			w.fail(ctx, taskID, classifyLoadError(err, errCodeVMNodeMissing), fmt.Errorf("load node: %v", err))
	}
	return vm, disks[0], tpl, pool, node, nil
}

// projectCreateSuccess writes the runtime row, bumps the template's
// derived_vm_count, and finalizes the task in one transaction.
func (w *VMCreateWorker) projectCreateSuccess(
	ctx context.Context, taskID uuid.UUID, args VMCreateArgs, result CreateResult,
) error {
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return w.fail(ctx, taskID, "internal", fmt.Errorf("marshal create result: %v", err))
	}
	nodeID := args.NodeID
	return w.store.InTx(ctx, func(q *store.Queries) error {
		if err := q.UpsertVMRuntime(ctx, store.UpsertVMRuntimeParams{
			VmID:               args.VMID,
			CurrentNodeID:      &nodeID,
			Phase:              store.VmPhaseRunning,
			ObservedGeneration: 1,
		}); err != nil {
			return fmt.Errorf("upsert vm_runtime: %v", err)
		}
		if err := q.IncrementTemplateDerivedVMCount(ctx, args.TemplateID); err != nil {
			return fmt.Errorf("increment derived_vm_count: %v", err)
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

// VMDeleteWorker drives the vm.delete lifecycle.
type VMDeleteWorker struct {
	river.WorkerDefaults[VMDeleteArgs]
	store    *store.Store
	executor DeleteExecutor
	logger   *slog.Logger
}

// Work executes one VMDeleteArgs job. Soft-delete chain (DeleteVMRuntime
// + SoftDeleteVMDisksByVM + SoftDeleteVM + DecrementTemplateDerivedVMCount)
// runs in a single InTx with the terminal task update — partial
// failure leaves the row in its prior state and the worker retries.
func (w *VMDeleteWorker) Work(ctx context.Context, j *river.Job[VMDeleteArgs]) error {
	if w.executor == nil {
		return fmt.Errorf("vm.delete executor not configured (Workers.Enabled likely false at boot)")
	}

	taskID := j.Args.TaskID

	if err := w.store.Queries().UpdateTaskRunning(ctx, taskID); err != nil {
		return fmt.Errorf("update task running: %v", err)
	}

	vm, node, err := w.loadDeleteEntities(ctx, taskID, j.Args)
	if err != nil {
		return err
	}

	task, err := w.store.Queries().GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("reload task: %v", err)
	}

	result, execErr := w.executor.Execute(ctx, DeleteArgs{
		TaskID:        taskID,
		AgentTaskID:   task.AgentTaskID,
		VMID:          j.Args.VMID,
		VMName:        vm.Name,
		Node:          node,
		OnAgentTaskID: w.persistAgentTaskID(taskID),
	})
	if execErr != nil {
		return w.fail(ctx, taskID, classifyVMError(execErr, errCodeVMDeleteFailed), execErr)
	}

	return w.projectDeleteSuccess(ctx, taskID, vm, result)
}

// loadDeleteEntities resolves the vm + node FKs.
func (w *VMDeleteWorker) loadDeleteEntities(
	ctx context.Context, taskID uuid.UUID, args VMDeleteArgs,
) (store.VM, store.Node, error) {
	vm, err := w.store.Queries().GetVMByID(ctx, args.VMID)
	if err != nil {
		return store.VM{}, store.Node{},
			w.fail(ctx, taskID, classifyLoadError(err, errCodeVMNotFound), fmt.Errorf("load vm: %v", err))
	}
	node, err := w.store.Queries().GetNodeByID(ctx, args.NodeID)
	if err != nil {
		return store.VM{}, store.Node{},
			w.fail(ctx, taskID, classifyLoadError(err, errCodeVMNodeMissing), fmt.Errorf("load node: %v", err))
	}
	return vm, node, nil
}

// projectDeleteSuccess runs the soft-delete chain inside one InTx
// alongside UpdateTaskFinalized. derived_vm_count is decremented
// only when vm.template_id is set — soft-delete-set-null on the
// template path leaves a nil here, in which case there's nothing
// to decrement.
func (w *VMDeleteWorker) projectDeleteSuccess(
	ctx context.Context, taskID uuid.UUID, vm store.VM, result DeleteResult,
) error {
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return w.fail(ctx, taskID, "internal", fmt.Errorf("marshal delete result: %v", err))
	}
	return w.store.InTx(ctx, func(q *store.Queries) error {
		if err := q.DeleteVMRuntime(ctx, vm.ID); err != nil {
			return fmt.Errorf("delete vm_runtime: %v", err)
		}
		if err := q.SoftDeleteVMDisksByVM(ctx, vm.ID); err != nil {
			return fmt.Errorf("soft delete vm_disks: %v", err)
		}
		if err := q.SoftDeleteVM(ctx, vm.ID); err != nil {
			return fmt.Errorf("soft delete vm: %v", err)
		}
		if vm.TemplateID != nil {
			if err := q.DecrementTemplateDerivedVMCount(ctx, *vm.TemplateID); err != nil {
				return fmt.Errorf("decrement derived_vm_count: %v", err)
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
}

// persistAgentTaskID returns the closure either worker invokes after
// the agent's 202. Shared by Create and Delete because the column
// shape is identical.
func (w *VMCreateWorker) persistAgentTaskID(taskID uuid.UUID) func(context.Context, uuid.UUID) error {
	return persistAgentTaskID(w.store, taskID)
}

func (w *VMDeleteWorker) persistAgentTaskID(taskID uuid.UUID) func(context.Context, uuid.UUID) error {
	return persistAgentTaskID(w.store, taskID)
}

func persistAgentTaskID(s *store.Store, taskID uuid.UUID) func(context.Context, uuid.UUID) error {
	return func(ctx context.Context, agentTaskID uuid.UUID) error {
		return s.Queries().UpdateTaskAgentTaskID(ctx, store.UpdateTaskAgentTaskIDParams{
			ID:          taskID,
			AgentTaskID: &agentTaskID,
		})
	}
}

// fail writes the terminal failed envelope and returns cause so river
// decides retry vs discard against MaxAttempts. Shared between
// VMCreateWorker and VMDeleteWorker via a free function so the worker
// types stay independent.
func (w *VMCreateWorker) fail(ctx context.Context, taskID uuid.UUID, code string, cause error) error {
	return failTask(ctx, w.store, w.logger, "vms.create", taskID, code, cause)
}

func (w *VMDeleteWorker) fail(ctx context.Context, taskID uuid.UUID, code string, cause error) error {
	return failTask(ctx, w.store, w.logger, "vms.delete", taskID, code, cause)
}

func failTask(
	ctx context.Context,
	s *store.Store,
	log *slog.Logger,
	op string,
	taskID uuid.UUID,
	code string,
	cause error,
) error {
	envelope, marshalErr := marshalTaskError(code, cause.Error())
	if marshalErr != nil {
		envelope = []byte(`{"code":"internal","message":"marshal error envelope failed"}`)
		log.ErrorContext(ctx, op+" marshal error envelope failed",
			"task_id", taskID, "code", code, "error", marshalErr)
	}
	if err := s.Queries().UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{
		ID:     taskID,
		Status: store.TaskStatusFailed,
		Result: nil,
		Error:  envelope,
	}); err != nil {
		log.ErrorContext(ctx, op+" finalize-failed write failed",
			"task_id", taskID, "code", code, "error", err)
		return fmt.Errorf("finalize failed: %v (cause: %v)", err, cause)
	}
	return cause
}

// marshalTaskError serialises a (code, message) pair into the JSONB
// payload the OpenAPI Task.error field surfaces.
func marshalTaskError(code, message string) ([]byte, error) {
	return json.Marshal(struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: message})
}

// classifyLoadError maps a pgx error to one of the *_not_found codes
// (when ErrNoRows) or "internal" for anything else.
func classifyLoadError(err error, notFoundCode string) string {
	if errors.Is(err, pgx.ErrNoRows) {
		return notFoundCode
	}
	return "internal"
}

// classifyVMError maps an executor error to a tasks.error.code.
// Agent envelope passthrough preserves agent-side codes verbatim;
// network / 5xx failures collapse to agent_unreachable; poll-budget
// exhaustion to request_timeout; everything else falls through to the
// supplied fallback (vm_create_failed / vm_delete_failed).
func classifyVMError(err error, fallback string) string {
	var ae *agentclient.AgentError
	if errors.As(err, &ae) {
		if ae.Code != "" {
			return ae.Code
		}
		return errCodeVMAgentUnreachabl
	}
	var te *agentclient.TimeoutError
	if errors.As(err, &te) {
		return errCodeVMTimeout
	}
	return fallback
}

// NewVMCreateWorker / NewVMDeleteWorker / Workers — registration
// helpers mirroring the storage_image.import pattern.

// NewVMCreateWorker constructs a VMCreateWorker. Exported so direct
// Worker.Work invocation in tests works without going through the
// river client.
func NewVMCreateWorker(deps CreateDeps) *VMCreateWorker {
	return &VMCreateWorker{store: deps.Store, executor: deps.Executor, logger: deps.Logger}
}

// NewVMDeleteWorker constructs a VMDeleteWorker.
func NewVMDeleteWorker(deps DeleteDeps) *VMDeleteWorker {
	return &VMDeleteWorker{store: deps.Store, executor: deps.Executor, logger: deps.Logger}
}

// Workers registers the vm.create and vm.delete workers on the bundle.
// Registration is unconditional — river requires the job kind to be
// in the workers registry for handler-side InsertTx to succeed even
// when the queue is never started. When deps.Executor is nil the
// registered worker still answers Insert but errors out cleanly if
// it is ever fetched (see Work()).
func Workers(workers *river.Workers, create CreateDeps, del DeleteDeps) {
	river.AddWorker(workers, NewVMCreateWorker(create))
	river.AddWorker(workers, NewVMDeleteWorker(del))
}

// templateChecksumHex projects the template's binary checksum to the
// hex-encoded form the agent's wire shape carries. The
// agentclient.VMCreateRequest's TemplateChecksum field is a
// 64-character hex string per the Iteration 1 agent's contract.
func templateChecksumHex(t store.Template) string {
	return hex.EncodeToString(t.ImageChecksumSha256)
}
