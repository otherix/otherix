// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// jobPresent reports whether the queue job with the given sequence still exists,
// mirroring the unexported jobKey zero-padding in the package under test.
func jobPresent(ctx context.Context, cli *etcd.Client, seq int64) (bool, error) {
	_, found, err := cli.Get(ctx, etcd.Key("jobs", fmt.Sprintf("%020d", seq)))
	return found, err
}

// pinnedUnobservedVM creates a scheduled VM pinned to nodeID with a full bind
// state (boot disk + create task + enqueued job) and, when netID is non-nil, a
// NIC on that network - but NO vm_runtime row, so it models a committed bind the
// agent never reported. It returns the VM id and the create task id.
func pinnedUnobservedVM(t *testing.T, s *etcdstore.Store, nodeID, poolID uuid.UUID, netID *uuid.UUID, mac string) (vmID, taskID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	owner := uuid.New()
	name := "vm-" + uuid.NewString()[:8]
	writes := vmCreateWrites(t, name, owner, nodeID, poolID)
	if netID != nil {
		writes.Nic = &store.CreateVMNicParams{
			ID: uuid.New(), VmID: writes.VM.ID, NetworkID: *netID, DeviceOrder: 0,
			Model: store.NicModelVirtio, MacAddress: mustMAC(t, mac),
		}
	}
	ct, err := s.CreateScheduledVM(ctx, func(store.PlacementReader) (store.VMCreateWrites, error) {
		return writes, nil
	})
	if err != nil {
		t.Fatalf("CreateScheduledVM: %v", err)
	}
	return writes.VM.ID, ct
}

// TestDeleteNodeNonForceCountsPinnedUnobservedVM is the HIGH-2 gate case: a VM
// pinned to the node with no vm_runtime row must still block a non-force delete.
// The pre-fix gate counted only the runtime-by-node index (empty here), so the
// delete would wrongly succeed and strand the VM's bind state.
func TestDeleteNodeNonForceCountsPinnedUnobservedVM(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	nodeID, poolID, _ := schedulingFixture(t, s)

	pinnedUnobservedVM(t, s, nodeID, poolID, nil, "")

	var blocking *store.ResourceInUseError
	if _, err := s.DeleteNode(ctx, nodeID, false, uuid.New()); !errors.As(err, &blocking) {
		t.Fatalf("DeleteNode(non-force) = %v, want *store.ResourceInUseError", err)
	}
	if blocking.Resources["vms"] != 1 {
		t.Errorf("blocking vms = %d, want 1 (the pinned-unobserved VM)", blocking.Resources["vms"])
	}
}

// TestDeleteNodeForceRollsBackPinnedUnobservedVM is the HIGH-2 rollback case: a
// force-delete of the node must return the pinned-unobserved VM to unscheduled
// and undo the whole bind (disk + disk indexes, NIC + per-network index + MAC
// guard, task, job), leaving desired_phase untouched, so nothing leaks. The NIC
// per-network index leak is the load-bearing one: without teardown the network
// is permanently undeletable.
func TestDeleteNodeForceRollsBackPinnedUnobservedVM(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	nodeID, poolID, _ := schedulingFixture(t, s)
	net := seedBridgeNetwork(t, s)

	vmID, taskID := pinnedUnobservedVM(t, s, nodeID, poolID, &net.ID, "52:54:00:aa:bb:01")

	before, err := s.VMByID(ctx, vmID)
	if err != nil {
		t.Fatalf("VMByID before: %v", err)
	}

	out, err := s.DeleteNode(ctx, nodeID, true, uuid.New())
	if err != nil {
		t.Fatalf("DeleteNode(force): %v", err)
	}
	if out.VMsRolledBack != 1 {
		t.Errorf("VMsRolledBack = %d, want 1", out.VMsRolledBack)
	}
	if out.VMsOrphaned != 0 {
		t.Errorf("VMsOrphaned = %d, want 0 (unobserved VM is rolled back, not orphaned)", out.VMsOrphaned)
	}

	// Node soft-deleted.
	if _, err := s.NodeByID(ctx, nodeID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("NodeByID after force-delete = %v, want ErrNotFound", err)
	}

	// VM back to unscheduled, unpinned, desired_phase untouched.
	vm, err := s.VMByID(ctx, vmID)
	if err != nil {
		t.Fatalf("VMByID after: %v", err)
	}
	if vm.SchedulingStatus != store.VMSchedulingUnscheduled {
		t.Errorf("SchedulingStatus = %q, want unscheduled", vm.SchedulingStatus)
	}
	if vm.PinnedNodeID != nil {
		t.Errorf("PinnedNodeID = %v, want nil", vm.PinnedNodeID)
	}
	if vm.DesiredPhase != before.DesiredPhase {
		t.Errorf("DesiredPhase = %q, want unchanged %q", vm.DesiredPhase, before.DesiredPhase)
	}

	// Re-added to the unscheduled index; dropped from the pinned index.
	unsched, err := s.ListUnscheduledVMs(ctx, 100)
	if err != nil {
		t.Fatalf("ListUnscheduledVMs: %v", err)
	}
	found := false
	for _, u := range unsched {
		if u.ID == vmID {
			found = true
		}
	}
	if !found {
		t.Errorf("VM %v not in unscheduled index, want present", vmID)
	}
	pinned, err := cli.Range(ctx, etcd.Key("index", "vms", "pinned_node", nodeID.String())+"/")
	if err != nil {
		t.Fatalf("range pinned index: %v", err)
	}
	if len(pinned) != 0 {
		t.Errorf("pinned index for node = %d entries, want 0", len(pinned))
	}

	// Boot disk torn down (row + vm-index + pool-index).
	disks, err := s.ListVMDisksByVM(ctx, vmID)
	if err != nil {
		t.Fatalf("ListVMDisksByVM: %v", err)
	}
	if len(disks) != 0 {
		t.Errorf("disks after rollback = %d, want 0", len(disks))
	}

	// NIC torn down: the per-network index is cleared, so the network is deletable.
	nics, err := s.ListVMNicsByVM(ctx, vmID)
	if err != nil {
		t.Fatalf("ListVMNicsByVM: %v", err)
	}
	if len(nics) != 0 {
		t.Errorf("nics after rollback = %d, want 0", len(nics))
	}
	if err := s.DeleteNetwork(ctx, net.ID); err != nil {
		t.Errorf("DeleteNetwork after rollback = %v, want nil (NIC per-network index must be cleared)", err)
	}

	// Create task settled (cancelled) and its job neutralized.
	task, err := s.TaskByID(ctx, taskID)
	if err != nil {
		t.Fatalf("TaskByID: %v", err)
	}
	if task.Status != store.TaskStatusCancelled {
		t.Errorf("create task status = %q, want cancelled", task.Status)
	}
	if task.JobID != nil {
		if found, err := jobPresent(ctx, cli, *task.JobID); err != nil {
			t.Fatalf("jobPresent: %v", err)
		} else if found {
			t.Errorf("create job %d still present, want neutralized", *task.JobID)
		}
	}
}

// TestDeleteNodeForceOrphansObservedVMKeepsDisk locks the destructive-routing
// invariant: a VM the agent HAS reported (vm_runtime row present) takes the
// orphan path (phase=orphaned, unpinned-runtime), and its boot disk MUST survive
// - it is never rolled back. Routing a running VM through the rollback would
// destroy a disk the operator still wants.
func TestDeleteNodeForceOrphansObservedVMKeepsDisk(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	nodeID, poolID, _ := schedulingFixture(t, s)

	vmID, createTask := pinnedUnobservedVM(t, s, nodeID, poolID, nil, "")
	// Make it observed: the agent reported a running runtime.
	if err := s.ProjectVMCreateSuccess(ctx,
		store.UpsertVMRuntimeParams{VmID: vmID, CurrentNodeID: &nodeID, Phase: store.VmPhaseRunning, ObservedGeneration: 1},
		store.UpdateTaskFinalizedParams{ID: createTask, Status: store.TaskStatusSuccess},
		nil,
	); err != nil {
		t.Fatalf("ProjectVMCreateSuccess: %v", err)
	}

	out, err := s.DeleteNode(ctx, nodeID, true, uuid.New())
	if err != nil {
		t.Fatalf("DeleteNode(force): %v", err)
	}
	if out.VMsOrphaned != 1 {
		t.Errorf("VMsOrphaned = %d, want 1", out.VMsOrphaned)
	}
	if out.VMsRolledBack != 0 {
		t.Errorf("VMsRolledBack = %d, want 0 (observed VM is orphaned, not rolled back)", out.VMsRolledBack)
	}

	var rt store.VMRuntime
	if _, err := cli.GetJSON(ctx, etcd.Key("vm_runtime", vmID.String()), &rt); err != nil {
		t.Fatalf("read vm_runtime: %v", err)
	}
	if rt.Phase != store.VmPhaseOrphaned || rt.CurrentNodeID != nil {
		t.Errorf("vm_runtime = %+v, want orphaned + nil node", rt)
	}

	// The boot disk survives - the running VM's disk must NOT be destroyed.
	disks, err := s.ListVMDisksByVM(ctx, vmID)
	if err != nil {
		t.Fatalf("ListVMDisksByVM: %v", err)
	}
	if len(disks) != 1 {
		t.Errorf("disks after orphan = %d, want 1 (disk preserved)", len(disks))
	}
}

// TestBindBlockedWhileNodeDeleting proves the producer guard: while a node's
// delete-intent key is present, a bind onto that node loses the guard CAS and
// leaves the VM unscheduled (the scheduler retries; once the node soft-deletes
// placement drops it).
func TestBindBlockedWhileNodeDeleting(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	nodeID, poolID, poolName := schedulingFixture(t, s)

	vmID, err := s.CreateUnscheduledVM(ctx, mkUnscheduledParams(t, "vm-nodedel"))
	if err != nil {
		t.Fatalf("CreateUnscheduledVM: %v", err)
	}

	// Simulate a node delete in progress.
	if err := cli.Put(ctx, etcd.Key("deleting", "nodes", nodeID.String()), []byte(time.Now().UTC().Format(time.RFC3339))); err != nil {
		t.Fatalf("seed node delete-intent: %v", err)
	}

	err = s.BindScheduledVM(ctx, vmID, func(pr store.PlacementReader) (store.VMBindWrites, error) {
		if _, perr := pr.ListEligiblePoolsByName(ctx, poolName); perr != nil {
			return store.VMBindWrites{}, perr
		}
		return store.VMBindWrites{
			PinnedNodeID: nodeID,
			Disk: store.CreateVMDiskParams{
				VmID: vmID, StoragePoolID: poolID, DeviceOrder: 0, Bus: store.DiskBusVirtio,
				SizeGib: 0, SourceKind: "image", Format: store.ImageFormatQcow2,
				CacheMode: store.DiskCacheModeWriteback, Discard: store.DiskDiscardUnmap,
			},
			Task: store.CreateTaskParams{
				ID: uuid.New(), Type: "vm.create", Status: store.TaskStatusPending,
				ResourceType: "vm", ResourceID: &vmID, Args: []byte(`{}`), MaxAttempts: 25,
			},
			Job: stubJobArgs{},
		}, nil
	})
	if !errors.Is(err, store.ErrVMNotUnscheduled) {
		t.Fatalf("BindScheduledVM into a deleting node = %v, want ErrVMNotUnscheduled", err)
	}
	vm, err := s.VMByID(ctx, vmID)
	if err != nil {
		t.Fatalf("VMByID: %v", err)
	}
	if vm.SchedulingStatus != store.VMSchedulingUnscheduled || vm.PinnedNodeID != nil {
		t.Errorf("vm = {status:%q pinned:%v}, want unscheduled + unpinned (bind must not commit)", vm.SchedulingStatus, vm.PinnedNodeID)
	}
}

// TestCutoverBlockedWhileTargetNodeDeleting proves the cutover producer guard: a
// cutover onto a node whose delete-intent is present loses the guard CAS
// (ErrConcurrentUpdate), so a force-delete of the target cannot be raced by a
// cutover that re-pins the VM onto the vanishing node.
func TestCutoverBlockedWhileTargetNodeDeleting(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	nodeA := nodeParams(uniqueNodeName("a"))
	nodeB := nodeParams(uniqueNodeName("b"))
	if _, err := s.CreateNode(ctx, nodeA); err != nil {
		t.Fatalf("CreateNode(A): %v", err)
	}
	if _, err := s.CreateNode(ctx, nodeB); err != nil {
		t.Fatalf("CreateNode(B): %v", err)
	}
	vm := seedPinnedVM(t, cli, nodeA.ID)
	m := seedActiveMigration(t, s, vm.ID, nodeA.ID, nodeB.ID)

	if err := cli.Put(ctx, etcd.Key("deleting", "nodes", nodeB.ID.String()), []byte(time.Now().UTC().Format(time.RFC3339))); err != nil {
		t.Fatalf("seed target node delete-intent: %v", err)
	}

	if err := s.CommitMigrationCutover(ctx, m.ID); !errors.Is(err, store.ErrConcurrentUpdate) {
		t.Fatalf("CommitMigrationCutover into a deleting target = %v, want ErrConcurrentUpdate", err)
	}
}

// TestCutoverRefusesSoftDeletedTargetNode is MED-2: after the target node row is
// soft-deleted (the delete-intent is gone by then), the cutover must still refuse
// rather than re-pin the VM onto a node that no longer exists.
func TestCutoverRefusesSoftDeletedTargetNode(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	nodeA := nodeParams(uniqueNodeName("a"))
	nodeB := nodeParams(uniqueNodeName("b"))
	if _, err := s.CreateNode(ctx, nodeA); err != nil {
		t.Fatalf("CreateNode(A): %v", err)
	}
	if _, err := s.CreateNode(ctx, nodeB); err != nil {
		t.Fatalf("CreateNode(B): %v", err)
	}
	vm := seedPinnedVM(t, cli, nodeA.ID)
	m := seedActiveMigration(t, s, vm.ID, nodeA.ID, nodeB.ID)

	// Soft-delete the target node row directly (row present, DeletedAt set).
	nb, err := s.NodeByID(ctx, nodeB.ID)
	if err != nil {
		t.Fatalf("NodeByID(B): %v", err)
	}
	now := time.Now().UTC()
	nb.DeletedAt = &now
	if err := cli.PutJSON(ctx, etcd.Key("nodes", nodeB.ID.String()), nb); err != nil {
		t.Fatalf("soft-delete node B: %v", err)
	}

	if err := s.CommitMigrationCutover(ctx, m.ID); !errors.Is(err, store.ErrMigrationTerminal) {
		t.Fatalf("CommitMigrationCutover onto soft-deleted target = %v, want ErrMigrationTerminal", err)
	}
}
