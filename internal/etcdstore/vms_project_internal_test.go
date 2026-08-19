// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// seedLifecycleTask writes a running task row the projections can finalize.
func seedLifecycleTask(t *testing.T, s *Store, typ string, vmID uuid.UUID) uuid.UUID {
	t.Helper()
	now := time.Now().UTC()
	task := store.Task{
		ID: uuid.New(), Type: typ, Status: store.TaskStatusRunning, ResourceType: "vm",
		ResourceID: &vmID, Args: []byte(`{}`), MaxAttempts: 3, CreatedAt: now, StartedAt: &now,
	}
	if err := s.c.PutJSON(context.Background(), taskKey(task.ID), task); err != nil {
		t.Fatalf("seed %s task: %v", typ, err)
	}
	return task.ID
}

// TestProjectVMLifecycleDoesNotResurrectADeletedVM drives the delete-during-
// lifecycle seam from inside the projection: a lifecycle op reads the VM row,
// a `vm delete` soft-deletes it, and only then does the lifecycle op commit its
// snapshot. The deletion stamp must survive - it is the teardown signal the
// heartbeat reads, and clearing it strands the guest permanently, because the
// VM is then neither declared anywhere nor tombstoned. The lifecycle task must
// still reach its terminal state: a dropped VM-row write may never cost the
// task its finalize, or it would hang in running forever.
//
// The read-to-commit window is not reachable from the external test package
// (nothing observable happens between the read and the commit), so the test
// drives the committing half directly with the stale snapshot and revision the
// exported projection would have been holding.
func TestProjectVMLifecycleDoesNotResurrectADeletedVM(t *testing.T) {
	s, _ := FreshStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	vm := store.VM{
		ID: uuid.New(), OwnerID: uuid.New(), Name: "vm-" + uuid.NewString()[:8],
		DesiredPhase: store.VmDesiredPhaseRunning, Architecture: store.CpuArchAmd64,
		CpuCores: 2, MemoryMib: 2048, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.c.PutJSON(ctx, vmKey(vm.ID), vm); err != nil {
		t.Fatalf("seed vm: %v", err)
	}
	stopTask := seedLifecycleTask(t, s, "vm.stop", vm.ID)
	deleteTask := seedLifecycleTask(t, s, "vm.delete", vm.ID)

	// The snapshot and revision a running `vm stop` projection holds.
	snap, rev, err := s.vmWithRev(ctx, vm.ID)
	if err != nil {
		t.Fatalf("vmWithRev: %v", err)
	}

	// A concurrent `vm delete` commits the soft-delete first.
	if err := s.ProjectVMDeleteSuccess(ctx, snap,
		store.UpdateTaskFinalizedParams{ID: deleteTask, Status: store.TaskStatusSuccess},
	); err != nil {
		t.Fatalf("ProjectVMDeleteSuccess: %v", err)
	}

	// The stop projection commits afterwards, still holding the pre-delete row.
	snap.DesiredPhase = store.VmDesiredPhaseStopped
	if err := s.projectVMLifecycle(ctx, vm.ID, &snap, rev, store.VmPhaseStopped,
		store.UpdateTaskFinalizedParams{ID: stopTask, Status: store.TaskStatusSuccess},
	); err != nil {
		t.Fatalf("projectVMLifecycle: %v", err)
	}

	deleted, name, err := s.VMSoftDeleted(ctx, vm.ID)
	if err != nil {
		t.Fatalf("VMSoftDeleted: %v", err)
	}
	if !deleted {
		t.Errorf("VMSoftDeleted(%s) = false, want true: the lifecycle projection cleared the deletion stamp with its stale snapshot", vm.Name)
	}
	if deleted && name != vm.Name {
		t.Errorf("VMSoftDeleted name = %q, want %q", name, vm.Name)
	}

	// The dropped VM-row write must not cost the task its finalize.
	task, err := s.TaskByID(ctx, stopTask)
	if err != nil {
		t.Fatalf("TaskByID: %v", err)
	}
	if task.Status != store.TaskStatusSuccess || task.FinishedAt == nil {
		t.Errorf("stop task = (status %v, finished %v), want success + finished", task.Status, task.FinishedAt)
	}
}

// TestProjectVMLifecycleRetriesAStaleLiveVMRow drives the same read-to-commit
// window against a VM that is still live: an unrelated writer bumps the row
// between the read and the commit, so the desired_phase put loses its compare.
// The projection is the only writer of desired_phase on the lifecycle path, so
// the intent must be re-applied to the re-read row, not dropped - a dropped put
// leaves the agent's reconciler driving the guest back to its previous phase.
// The runtime phase and the task finalize ride the losing branch and must land
// as well.
func TestProjectVMLifecycleRetriesAStaleLiveVMRow(t *testing.T) {
	s, _ := FreshStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	vm := store.VM{
		ID: uuid.New(), OwnerID: uuid.New(), Name: "vm-" + uuid.NewString()[:8],
		DesiredPhase: store.VmDesiredPhaseRunning, Architecture: store.CpuArchAmd64,
		CpuCores: 2, MemoryMib: 2048, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.c.PutJSON(ctx, vmKey(vm.ID), vm); err != nil {
		t.Fatalf("seed vm: %v", err)
	}
	nodeID := uuid.New()
	if err := s.c.PutJSON(ctx, vmRuntimeKey(vm.ID), store.VMRuntime{
		VmID: vm.ID, CurrentNodeID: &nodeID, Phase: store.VmPhaseRunning, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed vm runtime: %v", err)
	}
	stopTask := seedLifecycleTask(t, s, "vm.stop", vm.ID)

	// The snapshot and revision a running `vm stop` projection holds.
	snap, rev, err := s.vmWithRev(ctx, vm.ID)
	if err != nil {
		t.Fatalf("vmWithRev: %v", err)
	}

	// An unrelated writer bumps the row, staling the snapshot without deleting it.
	pinned := uuid.New()
	live := snap
	live.PinnedNodeID = &pinned
	if err := s.c.PutJSON(ctx, vmKey(vm.ID), live); err != nil {
		t.Fatalf("bump vm row: %v", err)
	}

	// The stop projection commits afterwards, still holding the pre-bump row.
	snap.DesiredPhase = store.VmDesiredPhaseStopped
	if err := s.projectVMLifecycle(ctx, vm.ID, &snap, rev, store.VmPhaseStopped,
		store.UpdateTaskFinalizedParams{ID: stopTask, Status: store.TaskStatusSuccess},
	); err != nil {
		t.Fatalf("projectVMLifecycle: %v", err)
	}

	got, err := s.VMByID(ctx, vm.ID)
	if err != nil {
		t.Fatalf("VMByID: %v", err)
	}
	if got.DesiredPhase != store.VmDesiredPhaseStopped {
		t.Errorf("desired_phase = %v, want %v: the lifecycle projection dropped the operator's intent", got.DesiredPhase, store.VmDesiredPhaseStopped)
	}
	if got.PinnedNodeID == nil || *got.PinnedNodeID != pinned {
		t.Errorf("pinned_node_id = %v, want %v: the retry must re-apply intent onto the re-read row", got.PinnedNodeID, pinned)
	}

	rt, err := s.VMRuntimeByID(ctx, vm.ID)
	if err != nil {
		t.Fatalf("VMRuntimeByID: %v", err)
	}
	if rt.Phase != store.VmPhaseStopped {
		t.Errorf("runtime phase = %v, want %v", rt.Phase, store.VmPhaseStopped)
	}

	task, err := s.TaskByID(ctx, stopTask)
	if err != nil {
		t.Fatalf("TaskByID: %v", err)
	}
	if task.Status != store.TaskStatusSuccess || task.FinishedAt == nil {
		t.Errorf("stop task = (status %v, finished %v), want success + finished", task.Status, task.FinishedAt)
	}
}
