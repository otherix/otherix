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

	vmshandlers "github.com/otherix/otherix/internal/api/handlers/vms"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// The etcd store satisfies the async VM worker handlers' storage contract - the
// seam the Run-form workers drive.
var _ vmshandlers.WorkerStore = (*etcdstore.Store)(nil)

func TestVMCreateRunHandlerSuccess(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	vmID, nodeID, poolID, templateID, taskID := seedCreatedVM(t, s)

	raw, _ := json.Marshal(vmshandlers.VMCreateArgs{TaskID: taskID, VMID: vmID, TemplateID: templateID, PoolID: poolID, NodeID: nodeID})
	h := vmshandlers.CreateHandler(s, &vmshandlers.StubVMCreateExecutor{Result: vmshandlers.CreateResult{VMID: vmID.String()}}, log)
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
	tpl, _ := s.TemplateByID(ctx, templateID)
	if tpl.DerivedVmCount != 1 {
		t.Errorf("derived_vm_count = %d, want 1", tpl.DerivedVmCount)
	}
}

func TestVMCreateRunHandlerExecutorErrorFinalizesFailed(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	vmID, nodeID, poolID, templateID, taskID := seedCreatedVM(t, s)

	raw, _ := json.Marshal(vmshandlers.VMCreateArgs{TaskID: taskID, VMID: vmID, TemplateID: templateID, PoolID: poolID, NodeID: nodeID})
	h := vmshandlers.CreateHandler(s, &vmshandlers.StubVMCreateExecutor{Err: errors.New("agent boom")}, log)
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

func TestVMDeleteRunHandlerSuccess(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	vmID, nodeID, _, templateID, createTask := seedCreatedVM(t, s)
	// Bring it to running first.
	if err := s.ProjectVMCreateSuccess(ctx,
		store.UpsertVMRuntimeParams{VmID: vmID, CurrentNodeID: &nodeID, Phase: store.VmPhaseRunning, ObservedGeneration: 1},
		templateID, store.UpdateTaskFinalizedParams{ID: createTask, Status: store.TaskStatusSuccess},
	); err != nil {
		t.Fatalf("seed running: %v", err)
	}

	delTask := taskParams(store.TaskStatusPending, nil)
	if _, err := s.EnqueueTask(ctx, delTask, testJobArgs{}); err != nil {
		t.Fatalf("enqueue delete: %v", err)
	}
	raw, _ := json.Marshal(vmshandlers.VMDeleteArgs{TaskID: delTask.ID, VMID: vmID, NodeID: nodeID})
	h := vmshandlers.DeleteHandler(s, &vmshandlers.StubVMDeleteExecutor{Result: vmshandlers.DeleteResult{VMID: vmID.String()}}, log)
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
	vmID, nodeID, _, templateID, createTask := seedCreatedVM(t, s)
	if err := s.ProjectVMCreateSuccess(ctx,
		store.UpsertVMRuntimeParams{VmID: vmID, CurrentNodeID: &nodeID, Phase: store.VmPhaseRunning, ObservedGeneration: 1},
		templateID, store.UpdateTaskFinalizedParams{ID: createTask, Status: store.TaskStatusSuccess},
	); err != nil {
		t.Fatalf("seed running: %v", err)
	}

	stopTask := taskParams(store.TaskStatusPending, nil)
	if _, err := s.EnqueueTask(ctx, stopTask, testJobArgs{}); err != nil {
		t.Fatalf("enqueue stop: %v", err)
	}
	raw, _ := json.Marshal(vmshandlers.VMStopArgs{TaskID: stopTask.ID, VMID: vmID, NodeID: nodeID})
	h := vmshandlers.LifecycleHandler(s, &vmshandlers.StubVMLifecycleExecutor{Result: vmshandlers.LifecycleResult{VMID: vmID.String()}},
		log, "stop", store.VmDesiredPhaseStopped, store.VmPhaseStopped, "vm_stop_failed")
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
