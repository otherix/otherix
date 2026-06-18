// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	vmshandlers "github.com/otherix/otherix/internal/api/handlers/vms"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// The etcd store satisfies the async VM worker handlers' storage contract - the
// seam the Run-form workers drive.
var _ vmshandlers.WorkerStore = (*etcdstore.Store)(nil)

// noopBroker is the SnapshotBlobBroker double for the image-model create tests
// in this file (no source snapshot, so the broker is never reached). BrokerPull
// always succeeds.
type noopBroker struct{}

func (noopBroker) BrokerPull(context.Context, string, uuid.UUID) error { return nil }

// createArgs builds the image-model vm.create job-args for the worker seam: the
// VM is self-describing (the worker reads the image source off the VM/disk rows),
// so the job payload carries only the row identifiers - no template id, no image
// fields.
func createArgs(taskID, vmID, poolID, nodeID uuid.UUID) vmshandlers.VMCreateArgs {
	return vmshandlers.VMCreateArgs{
		TaskID: taskID, VMID: vmID, PoolID: poolID, NodeID: nodeID,
	}
}

func TestVMCreateRunHandlerSuccess(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	vmID, nodeID, poolID, taskID := seedCreatedVM(t, s)
	// The create handler now treats a nil-heartbeat node as terminally dead;
	// bump the node fresh so this test exercises the live agent path.
	bumpHeartbeat(t, s, nodeID)

	raw, _ := json.Marshal(createArgs(taskID, vmID, poolID, nodeID))
	h := vmshandlers.CreateHandler(s, &vmshandlers.StubVMCreateExecutor{Result: vmshandlers.CreateResult{VMID: vmID.String()}}, noopBroker{}, log, 5*time.Minute)
	if err := h(ctx, raw); err != nil {
		t.Fatalf("create handler: %v", err)
	}

	task, _ := s.TaskByID(ctx, taskID)
	if task.Status != store.TaskStatusSuccess {
		t.Errorf("task = %v, want success", task.Status)
	}
	rt, err := s.VMRuntimeByID(ctx, vmID)
	if err != nil || rt.Phase != store.VmPhaseRunning {
		t.Errorf("runtime = (%+v, %v), want running", rt, err)
	}
}

func TestVMCreateRunHandlerExecutorErrorFinalizesFailed(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	vmID, nodeID, poolID, taskID := seedCreatedVM(t, s)
	// Fresh heartbeat so the create reaches the executor (live path); the
	// executor then errors, finalizing the task failed and returning the cause.
	bumpHeartbeat(t, s, nodeID)

	raw, _ := json.Marshal(createArgs(taskID, vmID, poolID, nodeID))
	h := vmshandlers.CreateHandler(s, &vmshandlers.StubVMCreateExecutor{Err: errors.New("agent boom")}, noopBroker{}, log, 5*time.Minute)
	if err := h(ctx, raw); err == nil {
		t.Fatalf("create handler = nil, want the executor error returned for requeue")
	}
	task, _ := s.TaskByID(ctx, taskID)
	if task.Status != store.TaskStatusFailed || task.FinishedAt == nil {
		t.Errorf("task = %+v, want failed + finished", task)
	}
	if _, err := s.VMRuntimeByID(ctx, vmID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("runtime exists after failed create = %v, want ErrNotFound", err)
	}
}

func TestVMCreateRunHandlerFailThenSucceedProjects(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	vmID, nodeID, poolID, taskID := seedCreatedVM(t, s)
	// Fresh heartbeat so both deliveries take the live agent path rather than the
	// terminally-dead short-circuit.
	bumpHeartbeat(t, s, nodeID)

	raw, _ := json.Marshal(createArgs(taskID, vmID, poolID, nodeID))

	// Delivery 1: the executor errors, failRun finalizes the task to failed (a
	// retryable terminal), and the dispatcher would requeue the job.
	h1 := vmshandlers.CreateHandler(s, &vmshandlers.StubVMCreateExecutor{Err: errors.New("agent boom")}, noopBroker{}, log, 5*time.Minute)
	if err := h1(ctx, raw); err == nil {
		t.Fatalf("create handler (delivery 1) = nil, want the executor error for requeue")
	}
	task, _ := s.TaskByID(ctx, taskID)
	if task.Status != store.TaskStatusFailed {
		t.Fatalf("task after delivery 1 = %v, want failed", task.Status)
	}

	// Delivery 2 (job redelivered): the executor now succeeds. A failed task is
	// retryable, so the projection MUST run: runtime row present, count==1, task
	// ends success.
	h2 := vmshandlers.CreateHandler(s, &vmshandlers.StubVMCreateExecutor{Result: vmshandlers.CreateResult{VMID: vmID.String()}}, noopBroker{}, log, 5*time.Minute)
	if err := h2(ctx, raw); err != nil {
		t.Fatalf("create handler (delivery 2) = %v, want nil", err)
	}

	rt, err := s.VMRuntimeByID(ctx, vmID)
	if err != nil || rt.Phase != store.VmPhaseRunning {
		t.Errorf("runtime after fail-then-succeed = (%+v, %v), want running", rt, err)
	}
	task, _ = s.TaskByID(ctx, taskID)
	if task.Status != store.TaskStatusSuccess {
		t.Errorf("task after fail-then-succeed = %v, want success", task.Status)
	}
}

func TestVMDeleteRunHandlerFailThenSucceedProjects(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	vmID, nodeID, _, createTask := seedCreatedVM(t, s)
	// A node hosting a running VM has heartbeated recently: bump it fresh so the
	// delete handler exercises the agent path rather than the staleness
	// terminal-death arm.
	bumpHeartbeat(t, s, nodeID)
	// Bring it to running so delete has a runtime row to tear down.
	if err := s.ProjectVMCreateSuccess(ctx,
		store.UpsertVMRuntimeParams{VmID: vmID, CurrentNodeID: &nodeID, Phase: store.VmPhaseRunning, ObservedGeneration: 1},
		store.UpdateTaskFinalizedParams{ID: createTask, Status: store.TaskStatusSuccess},
		nil,
	); err != nil {
		t.Fatalf("seed running: %v", err)
	}

	delTask := taskParams(store.TaskStatusPending, nil)
	if _, err := s.EnqueueTask(ctx, delTask, testJobArgs{}); err != nil {
		t.Fatalf("enqueue delete: %v", err)
	}
	raw, _ := json.Marshal(vmshandlers.VMDeleteArgs{TaskID: delTask.ID, VMID: vmID, NodeID: nodeID})

	// Delivery 1: the executor errors, failRun finalizes the task failed.
	h1 := vmshandlers.DeleteHandler(s, &vmshandlers.StubVMDeleteExecutor{Err: errors.New("agent boom")}, log, 5*time.Minute)
	if err := h1(ctx, raw); err == nil {
		t.Fatalf("delete handler (delivery 1) = nil, want the executor error for requeue")
	}
	task, _ := s.TaskByID(ctx, delTask.ID)
	if task.Status != store.TaskStatusFailed {
		t.Fatalf("task after delivery 1 = %v, want failed", task.Status)
	}

	// Delivery 2 (job redelivered): the executor now succeeds. The delete
	// projection MUST run: VM gone, count decremented to 0, task ends success.
	h2 := vmshandlers.DeleteHandler(s, &vmshandlers.StubVMDeleteExecutor{Result: vmshandlers.DeleteResult{VMID: vmID.String()}}, log, 5*time.Minute)
	if err := h2(ctx, raw); err != nil {
		t.Fatalf("delete handler (delivery 2) = %v, want nil", err)
	}

	if _, err := s.VMByID(ctx, vmID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("vm after fail-then-succeed = %v, want ErrNotFound", err)
	}
	task, _ = s.TaskByID(ctx, delTask.ID)
	if task.Status != store.TaskStatusSuccess {
		t.Errorf("task after fail-then-succeed = %v, want success", task.Status)
	}
}

func TestVMDeleteRunHandlerSuccess(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	vmID, nodeID, _, createTask := seedCreatedVM(t, s)
	// A node hosting a running VM has heartbeated recently: bump it fresh so the
	// delete handler exercises the agent path rather than the staleness
	// terminal-death arm.
	bumpHeartbeat(t, s, nodeID)
	// Bring it to running first.
	if err := s.ProjectVMCreateSuccess(ctx,
		store.UpsertVMRuntimeParams{VmID: vmID, CurrentNodeID: &nodeID, Phase: store.VmPhaseRunning, ObservedGeneration: 1},
		store.UpdateTaskFinalizedParams{ID: createTask, Status: store.TaskStatusSuccess},
		nil,
	); err != nil {
		t.Fatalf("seed running: %v", err)
	}

	delTask := taskParams(store.TaskStatusPending, nil)
	if _, err := s.EnqueueTask(ctx, delTask, testJobArgs{}); err != nil {
		t.Fatalf("enqueue delete: %v", err)
	}
	raw, _ := json.Marshal(vmshandlers.VMDeleteArgs{TaskID: delTask.ID, VMID: vmID, NodeID: nodeID})
	h := vmshandlers.DeleteHandler(s, &vmshandlers.StubVMDeleteExecutor{Result: vmshandlers.DeleteResult{VMID: vmID.String()}}, log, 5*time.Minute)
	if err := h(ctx, raw); err != nil {
		t.Fatalf("delete handler: %v", err)
	}
	if _, err := s.VMByID(ctx, vmID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("vm still present after delete = %v, want ErrNotFound", err)
	}
	task, _ := s.TaskByID(ctx, delTask.ID)
	if task.Status != store.TaskStatusSuccess {
		t.Errorf("delete task = %v, want success", task.Status)
	}
}

func TestVMLifecycleRunHandlerStop(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	vmID, nodeID, _, createTask := seedCreatedVM(t, s)
	// A node hosting a running VM has heartbeated recently: bump it fresh so the
	// lifecycle handler exercises the agent path rather than the terminally-dead
	// short-circuit.
	bumpHeartbeat(t, s, nodeID)
	if err := s.ProjectVMCreateSuccess(ctx,
		store.UpsertVMRuntimeParams{VmID: vmID, CurrentNodeID: &nodeID, Phase: store.VmPhaseRunning, ObservedGeneration: 1},
		store.UpdateTaskFinalizedParams{ID: createTask, Status: store.TaskStatusSuccess},
		nil,
	); err != nil {
		t.Fatalf("seed running: %v", err)
	}

	stopTask := taskParams(store.TaskStatusPending, nil)
	if _, err := s.EnqueueTask(ctx, stopTask, testJobArgs{}); err != nil {
		t.Fatalf("enqueue stop: %v", err)
	}
	raw, _ := json.Marshal(vmshandlers.VMStopArgs{TaskID: stopTask.ID, VMID: vmID, NodeID: nodeID})
	h := vmshandlers.LifecycleHandler(s, &vmshandlers.StubVMLifecycleExecutor{Result: vmshandlers.LifecycleResult{VMID: vmID.String()}},
		log, "stop", store.VmDesiredPhaseStopped, store.VmPhaseStopped, "vm_stop_failed", 5*time.Minute)
	if err := h(ctx, raw); err != nil {
		t.Fatalf("stop handler: %v", err)
	}
	vm, _ := s.VMByID(ctx, vmID)
	if vm.DesiredPhase != store.VmDesiredPhaseStopped {
		t.Errorf("desired_phase = %v, want stopped", vm.DesiredPhase)
	}
	rt, _ := s.VMRuntimeByID(ctx, vmID)
	if rt.Phase != store.VmPhaseStopped {
		t.Errorf("runtime phase = %v, want stopped", rt.Phase)
	}
}
