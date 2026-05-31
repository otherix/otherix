// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// seedCreatedVM seeds a scheduled VM (vm + disk + pending task) and returns the
// ids the projection methods key off, plus the create task id.
func seedCreatedVM(t *testing.T, s *etcdstore.Store) (vmID, nodeID, poolID, templateID, taskID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	nodeID, poolID, templateID, _ = schedulingFixture(t, s)
	owner := uuid.New()
	name := "vm-" + uuid.NewString()[:8]
	var writes store.VMCreateWrites
	taskID, err := s.CreateScheduledVM(ctx, func(store.PlacementReader) (store.VMCreateWrites, error) {
		writes = vmCreateWrites(t, name, owner, nodeID, poolID, templateID)
		return writes, nil
	})
	if err != nil {
		t.Fatalf("CreateScheduledVM: %v", err)
	}
	return writes.VM.ID, nodeID, poolID, templateID, taskID
}

func TestProjectVMCreateSuccess(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	vmID, nodeID, _, templateID, taskID := seedCreatedVM(t, s)

	if err := s.UpdateTaskRunning(ctx, taskID); err != nil {
		t.Fatalf("UpdateTaskRunning: %v", err)
	}
	if err := s.ProjectVMCreateSuccess(ctx,
		store.UpsertVMRuntimeParams{
			VmID:               vmID,
			CurrentNodeID:      &nodeID,
			Phase:              store.VmPhaseRunning,
			ObservedGeneration: 1,
		},
		templateID,
		store.UpdateTaskFinalizedParams{ID: taskID, Status: store.TaskStatusSuccess, Result: []byte(`{"vm_id":"x"}`)},
	); err != nil {
		t.Fatalf("ProjectVMCreateSuccess: %v", err)
	}

	rt, err := s.VMRuntimeByID(ctx, vmID)
	if err != nil || rt.Phase != store.VmPhaseRunning || rt.CurrentNodeID == nil || *rt.CurrentNodeID != nodeID {
		t.Errorf("runtime = (%+v, %v), want running on node %v", rt, err, nodeID)
	}
	tpl, err := s.TemplateByID(ctx, templateID)
	if err != nil || tpl.DerivedVmCount != 1 {
		t.Errorf("template derived_vm_count = (%d, %v), want 1", tpl.DerivedVmCount, err)
	}
	task, err := s.TaskByID(ctx, taskID)
	if err != nil || task.Status != store.TaskStatusSuccess || task.FinishedAt == nil {
		t.Errorf("task = (%+v, %v), want success + finished", task, err)
	}
}

func TestProjectVMDeleteSuccess(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	vmID, nodeID, poolID, templateID, createTask := seedCreatedVM(t, s)
	// Bring it to running (runtime row + derived_vm_count=1) so delete has
	// something to tear down and a count to decrement.
	if err := s.ProjectVMCreateSuccess(ctx,
		store.UpsertVMRuntimeParams{VmID: vmID, CurrentNodeID: &nodeID, Phase: store.VmPhaseRunning, ObservedGeneration: 1},
		templateID,
		store.UpdateTaskFinalizedParams{ID: createTask, Status: store.TaskStatusSuccess},
	); err != nil {
		t.Fatalf("seed create projection: %v", err)
	}
	vm, err := s.VMByID(ctx, vmID)
	if err != nil {
		t.Fatalf("VMByID: %v", err)
	}
	name := vm.Name

	delTask := taskParams(store.TaskStatusPending, nil)
	if _, err := s.EnqueueTask(ctx, delTask, testJobArgs{}); err != nil {
		t.Fatalf("EnqueueTask(delete): %v", err)
	}
	if err := s.ProjectVMDeleteSuccess(ctx, vm,
		store.UpdateTaskFinalizedParams{ID: delTask.ID, Status: store.TaskStatusSuccess},
	); err != nil {
		t.Fatalf("ProjectVMDeleteSuccess: %v", err)
	}

	// VM is soft-deleted: gone from reads, name reusable, runtime gone, disks gone.
	if _, err := s.VMByID(ctx, vmID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("VMByID after delete = %v, want ErrNotFound", err)
	}
	if _, err := s.VMByName(ctx, name); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("VMByName after delete = %v, want ErrNotFound (guard dropped)", err)
	}
	if _, err := s.VMRuntimeByID(ctx, vmID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("VMRuntimeByID after delete = %v, want ErrNotFound", err)
	}
	disks, _ := s.ListVMDisksByVM(ctx, vmID)
	if len(disks) != 0 {
		t.Errorf("disks after delete = %d, want 0", len(disks))
	}
	// derived_vm_count decremented back to 0.
	tpl, _ := s.TemplateByID(ctx, templateID)
	if tpl.DerivedVmCount != 0 {
		t.Errorf("derived_vm_count after delete = %d, want 0", tpl.DerivedVmCount)
	}
	// Pool no longer blocked by the (now-deleted) disk: delete must succeed.
	if err := s.DeleteStoragePool(ctx, poolID); err != nil {
		t.Errorf("DeleteStoragePool after vm delete = %v, want nil (disk pool-index dropped)", err)
	}
	task, _ := s.TaskByID(ctx, delTask.ID)
	if task.Status != store.TaskStatusSuccess || task.FinishedAt == nil {
		t.Errorf("delete task = %+v, want success + finished", task)
	}
}

func TestProjectVMLifecycleSuccess(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	vmID, nodeID, _, templateID, createTask := seedCreatedVM(t, s)
	// Bring the VM to running so a runtime row exists for the lifecycle phase write.
	if err := s.ProjectVMCreateSuccess(ctx,
		store.UpsertVMRuntimeParams{VmID: vmID, CurrentNodeID: &nodeID, Phase: store.VmPhaseRunning, ObservedGeneration: 1},
		templateID,
		store.UpdateTaskFinalizedParams{ID: createTask, Status: store.TaskStatusSuccess},
	); err != nil {
		t.Fatalf("seed create projection: %v", err)
	}

	// A fresh stop task, then project the lifecycle success.
	stopTask := taskParams(store.TaskStatusPending, nil)
	if _, err := s.EnqueueTask(ctx, stopTask, testJobArgs{}); err != nil {
		t.Fatalf("EnqueueTask(stop): %v", err)
	}
	if err := s.ProjectVMLifecycleSuccess(ctx, vmID,
		store.VmDesiredPhaseStopped, store.VmPhaseStopped,
		store.UpdateTaskFinalizedParams{ID: stopTask.ID, Status: store.TaskStatusSuccess},
	); err != nil {
		t.Fatalf("ProjectVMLifecycleSuccess: %v", err)
	}

	vm, _ := s.VMByID(ctx, vmID)
	if vm.DesiredPhase != store.VmDesiredPhaseStopped {
		t.Errorf("desired_phase = %v, want stopped", vm.DesiredPhase)
	}
	rt, _ := s.VMRuntimeByID(ctx, vmID)
	if rt.Phase != store.VmPhaseStopped {
		t.Errorf("runtime phase = %v, want stopped", rt.Phase)
	}
	task, _ := s.TaskByID(ctx, stopTask.ID)
	if task.Status != store.TaskStatusSuccess || task.FinishedAt == nil {
		t.Errorf("stop task = %+v, want success + finished", task)
	}

	// reboot keeps desired_phase unchanged (empty desiredPhase = skip).
	rebootTask := taskParams(store.TaskStatusPending, nil)
	if _, err := s.EnqueueTask(ctx, rebootTask, testJobArgs{}); err != nil {
		t.Fatalf("EnqueueTask(reboot): %v", err)
	}
	if err := s.ProjectVMLifecycleSuccess(ctx, vmID,
		store.VMDesiredPhase(""), store.VmPhaseRunning,
		store.UpdateTaskFinalizedParams{ID: rebootTask.ID, Status: store.TaskStatusSuccess},
	); err != nil {
		t.Fatalf("ProjectVMLifecycleSuccess(reboot): %v", err)
	}
	vm, _ = s.VMByID(ctx, vmID)
	if vm.DesiredPhase != store.VmDesiredPhaseStopped {
		t.Errorf("desired_phase after reboot = %v, want unchanged (stopped)", vm.DesiredPhase)
	}
	rt, _ = s.VMRuntimeByID(ctx, vmID)
	if rt.Phase != store.VmPhaseRunning {
		t.Errorf("runtime phase after reboot = %v, want running", rt.Phase)
	}
}
