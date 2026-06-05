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

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// seedCreatedVM seeds a scheduled VM (vm + disk + pending task) and returns the
// ids the projection methods key off, plus the create task id.
func seedCreatedVM(t *testing.T, s *etcdstore.Store) (vmID, nodeID, poolID, taskID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	nodeID, poolID, _ = schedulingFixture(t, s)
	owner := uuid.New()
	name := "vm-" + uuid.NewString()[:8]
	var writes store.VMCreateWrites
	taskID, err := s.CreateScheduledVM(ctx, func(store.PlacementReader) (store.VMCreateWrites, error) {
		writes = vmCreateWrites(t, name, owner, nodeID, poolID)
		return writes, nil
	})
	if err != nil {
		t.Fatalf("CreateScheduledVM: %v", err)
	}
	return writes.VM.ID, nodeID, poolID, taskID
}

func TestProjectVMCreateSuccess(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	vmID, nodeID, _, taskID := seedCreatedVM(t, s)

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
		store.UpdateTaskFinalizedParams{ID: taskID, Status: store.TaskStatusSuccess, Result: []byte(`{"vm_id":"x"}`)},
	); err != nil {
		t.Fatalf("ProjectVMCreateSuccess: %v", err)
	}

	rt, err := s.VMRuntimeByID(ctx, vmID)
	if err != nil || rt.Phase != store.VmPhaseRunning || rt.CurrentNodeID == nil || *rt.CurrentNodeID != nodeID {
		t.Errorf("runtime = (%+v, %v), want running on node %v", rt, err, nodeID)
	}
	// The projection must not clobber the self-describing image fields the
	// create-write path stamped onto the VM row.
	vm, err := s.VMByID(ctx, vmID)
	if err != nil || vm.ImageURL == "" || vm.ImageFormat == "" {
		t.Errorf("vm image fields = (url=%q, format=%q, err=%v), want url+format set by create write", vm.ImageURL, vm.ImageFormat, err)
	}
	task, err := s.TaskByID(ctx, taskID)
	if err != nil || task.Status != store.TaskStatusSuccess || task.FinishedAt == nil {
		t.Errorf("task = (%+v, %v), want success + finished", task, err)
	}
}

func TestProjectVMCreateSuccessIdempotentOnRedelivery(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	vmID, nodeID, _, taskID := seedCreatedVM(t, s)

	if err := s.UpdateTaskRunning(ctx, taskID); err != nil {
		t.Fatalf("UpdateTaskRunning: %v", err)
	}
	rt := store.UpsertVMRuntimeParams{
		VmID:               vmID,
		CurrentNodeID:      &nodeID,
		Phase:              store.VmPhaseRunning,
		ObservedGeneration: 1,
	}
	fin := store.UpdateTaskFinalizedParams{ID: taskID, Status: store.TaskStatusSuccess, Result: []byte(`{"vm_id":"x"}`)}

	if err := s.ProjectVMCreateSuccess(ctx, rt, fin); err != nil {
		t.Fatalf("ProjectVMCreateSuccess (first): %v", err)
	}
	// Worker redelivery: an unconditional put of the same runtime + finalized
	// task value re-writes identical bytes, so the end state is unchanged.
	if err := s.ProjectVMCreateSuccess(ctx, rt, fin); err != nil {
		t.Fatalf("ProjectVMCreateSuccess (redelivery): %v", err)
	}

	rtRow, err := s.VMRuntimeByID(ctx, vmID)
	if err != nil || rtRow.Phase != store.VmPhaseRunning {
		t.Errorf("runtime after redelivery = (%+v, %v), want running", rtRow, err)
	}
	task, _ := s.TaskByID(ctx, taskID)
	if task.Status != store.TaskStatusSuccess {
		t.Errorf("task status after redelivery = %v, want success", task.Status)
	}
}

func TestProjectVMDeleteSuccessIdempotentOnRedelivery(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	vmID, nodeID, _, createTask := seedCreatedVM(t, s)
	if err := s.ProjectVMCreateSuccess(ctx,
		store.UpsertVMRuntimeParams{VmID: vmID, CurrentNodeID: &nodeID, Phase: store.VmPhaseRunning, ObservedGeneration: 1},
		store.UpdateTaskFinalizedParams{ID: createTask, Status: store.TaskStatusSuccess},
	); err != nil {
		t.Fatalf("seed create projection: %v", err)
	}
	vm, err := s.VMByID(ctx, vmID)
	if err != nil {
		t.Fatalf("VMByID: %v", err)
	}

	delTask := taskParams(store.TaskStatusPending, nil)
	if _, err := s.EnqueueTask(ctx, delTask, testJobArgs{}); err != nil {
		t.Fatalf("EnqueueTask(delete): %v", err)
	}
	fin := store.UpdateTaskFinalizedParams{ID: delTask.ID, Status: store.TaskStatusSuccess}

	if err := s.ProjectVMDeleteSuccess(ctx, vm, fin); err != nil {
		t.Fatalf("ProjectVMDeleteSuccess (first): %v", err)
	}
	// Worker redelivery: re-applying the same soft-delete + finalize is an
	// unconditional put of identical bytes, so the VM stays deleted and the
	// task stays success (no negative drift on any derived state).
	if err := s.ProjectVMDeleteSuccess(ctx, vm, fin); err != nil {
		t.Fatalf("ProjectVMDeleteSuccess (redelivery): %v", err)
	}

	if _, err := s.VMByID(ctx, vmID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("VMByID after redelivery = %v, want ErrNotFound", err)
	}
	task, _ := s.TaskByID(ctx, delTask.ID)
	if task.Status != store.TaskStatusSuccess {
		t.Errorf("task status after redelivery = %v, want success", task.Status)
	}
}

func TestProjectVMDeleteSuccess(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	vmID, nodeID, poolID, createTask := seedCreatedVM(t, s)
	// Bring it to running (runtime row) so delete has something to tear down.
	if err := s.ProjectVMCreateSuccess(ctx,
		store.UpsertVMRuntimeParams{VmID: vmID, CurrentNodeID: &nodeID, Phase: store.VmPhaseRunning, ObservedGeneration: 1},
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
	// Pool no longer blocked by the (now-deleted) disk: delete must succeed.
	if err := s.DeleteStoragePool(ctx, poolID); err != nil {
		t.Errorf("DeleteStoragePool after vm delete = %v, want nil (disk pool-index dropped)", err)
	}
	task, _ := s.TaskByID(ctx, delTask.ID)
	if task.Status != store.TaskStatusSuccess || task.FinishedAt == nil {
		t.Errorf("delete task = %+v, want success + finished", task)
	}
}

// TestProjectVMDeleteReleasesRuntimeNodeIndex verifies the delete projection
// drops the vm_runtime-by-node secondary index entry the agent heartbeat wrote,
// so a later DeleteNode does not see a phantom vms blocker. The index entry is
// maintained only by the heartbeat path (reindexRuntimeNode), so the test seeds
// it through UpsertVMRuntime, the real flow that binds a VM to a node.
func TestProjectVMDeleteReleasesRuntimeNodeIndex(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	vmID, nodeID, _, createTask := seedCreatedVM(t, s)
	if err := s.ProjectVMCreateSuccess(ctx,
		store.UpsertVMRuntimeParams{VmID: vmID, CurrentNodeID: &nodeID, Phase: store.VmPhaseRunning, ObservedGeneration: 1},
		store.UpdateTaskFinalizedParams{ID: createTask, Status: store.TaskStatusSuccess},
	); err != nil {
		t.Fatalf("seed create projection: %v", err)
	}
	// The agent heartbeat is what writes the vm_runtime-by-node index entry
	// (reindexRuntimeNode). Seed it the way the heartbeat does on first bind.
	idxKey := etcd.Key("index", "vm_runtime", "node", nodeID.String(), vmID.String())
	if err := cli.Put(ctx, idxKey, []byte(vmID.String())); err != nil {
		t.Fatalf("seed runtime node index: %v", err)
	}

	idxPrefix := etcd.Key("index", "vm_runtime", "node", nodeID.String()) + "/"
	if got := countIndex(t, cli, idxPrefix); got != 1 {
		t.Fatalf("vm_runtime node index before delete = %d, want 1", got)
	}

	vm, err := s.VMByID(ctx, vmID)
	if err != nil {
		t.Fatalf("VMByID: %v", err)
	}
	delTask := taskParams(store.TaskStatusPending, nil)
	if _, err := s.EnqueueTask(ctx, delTask, testJobArgs{}); err != nil {
		t.Fatalf("EnqueueTask(delete): %v", err)
	}
	if err := s.ProjectVMDeleteSuccess(ctx, vm,
		store.UpdateTaskFinalizedParams{ID: delTask.ID, Status: store.TaskStatusSuccess},
	); err != nil {
		t.Fatalf("ProjectVMDeleteSuccess: %v", err)
	}

	if got := countIndex(t, cli, idxPrefix); got != 0 {
		t.Errorf("vm_runtime node index after delete = %d, want 0 (released)", got)
	}
	// Consequence: the node is now deletable without force - no phantom vms blocker.
	if _, err := s.DeleteNode(ctx, nodeID, false, uuid.New()); err != nil {
		t.Errorf("DeleteNode(non-force) after vm delete = %v, want nil (no phantom blocker)", err)
	}
}

// countIndex returns the number of keys under the given etcd prefix.
func countIndex(t *testing.T, cli *etcd.Client, prefix string) int {
	t.Helper()
	items, err := cli.Range(context.Background(), prefix)
	if err != nil {
		t.Fatalf("Range(%q): %v", prefix, err)
	}
	return len(items)
}

func TestProjectVMLifecycleSuccess(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	vmID, nodeID, _, createTask := seedCreatedVM(t, s)
	// Bring the VM to running so a runtime row exists for the lifecycle phase write.
	if err := s.ProjectVMCreateSuccess(ctx,
		store.UpsertVMRuntimeParams{VmID: vmID, CurrentNodeID: &nodeID, Phase: store.VmPhaseRunning, ObservedGeneration: 1},
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
