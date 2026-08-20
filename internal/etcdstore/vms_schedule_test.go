// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration

package etcdstore_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// stubJobArgs is a minimal queue.JobArgs whose Kind matches the vm.create job
// enqueued by BindScheduledVM.
type stubJobArgs struct{}

func (stubJobArgs) Kind() string { return "vm.create" }

func mkUnscheduledParams(t *testing.T, name string) store.CreateVMParams {
	t.Helper()
	spec, err := store.MarshalSchedulingSpec(store.SchedulingSpec{PoolName: "default", DiskGiB: 0})
	if err != nil {
		t.Fatalf("MarshalSchedulingSpec: %v", err)
	}
	return store.CreateVMParams{
		ID:             uuid.New(),
		OwnerID:        uuid.New(),
		Name:           name,
		Architecture:   store.CpuArchAmd64,
		ImageURL:       "https://example.com/img.qcow2",
		ImageFormat:    store.ImageFormatQcow2,
		CpuCores:       2,
		MemoryMib:      2048,
		CPUModel:       "host",
		MachineType:    "q35",
		Labels:         []byte(`{}`),
		SchedulingSpec: spec,
	}
}

func TestCreateVMRoundTripsNetworkConfig(t *testing.T) {
	st, _ := etcdstore.FreshStore(t)
	ctx := context.Background()

	nc := "network:\n  version: 2\n"
	p := mkUnscheduledParams(t, "vm-netcfg")
	p.NetworkConfig = &nc
	id, err := st.CreateUnscheduledVM(ctx, p)
	if err != nil {
		t.Fatalf("CreateUnscheduledVM: %v", err)
	}

	vm, err := st.VMByID(ctx, id)
	if err != nil {
		t.Fatalf("VMByID: %v", err)
	}
	if vm.NetworkConfig == nil || *vm.NetworkConfig != nc {
		t.Errorf("VMByID().NetworkConfig = %v, want %q", vm.NetworkConfig, nc)
	}
}

// TestCreateVM_FromSnapshot_SetsProvenanceEmptyImage locks the recreate
// provenance invariant at the store seam: a VM created with SourceSnapshotID set
// persists SourceSnapshotID != nil and carries no image lineage (ImageURL == ""
// and ImageSHA256 == nil), even if the params carried image fields - a
// snapshot-sourced VM's disk is the snapshot state, not a base image.
func TestCreateVM_FromSnapshot_SetsProvenanceEmptyImage(t *testing.T) {
	st, _ := etcdstore.FreshStore(t)
	ctx := context.Background()

	snapID := uuid.New()
	p := mkUnscheduledParams(t, "vm-from-snap")
	p.SourceSnapshotID = &snapID
	// Defense in depth: even if the handler left these populated, the store must
	// drop them for a snapshot-sourced VM.
	p.ImageURL = "https://example.com/base.qcow2"
	p.ImageSHA256 = []byte{0xde, 0xad, 0xbe, 0xef}

	id, err := st.CreateUnscheduledVM(ctx, p)
	if err != nil {
		t.Fatalf("CreateUnscheduledVM: %v", err)
	}
	vm, err := st.VMByID(ctx, id)
	if err != nil {
		t.Fatalf("VMByID: %v", err)
	}
	if vm.SourceSnapshotID == nil || *vm.SourceSnapshotID != snapID {
		t.Errorf("SourceSnapshotID = %v, want %v", vm.SourceSnapshotID, snapID)
	}
	if vm.ImageURL != "" {
		t.Errorf("ImageURL = %q, want empty (snapshot-sourced)", vm.ImageURL)
	}
	if vm.ImageSHA256 != nil {
		t.Errorf("ImageSHA256 = %v, want nil (snapshot-sourced)", vm.ImageSHA256)
	}
}

func TestCreateUnscheduledVM(t *testing.T) {
	st, _ := etcdstore.FreshStore(t)
	ctx := context.Background()

	p := mkUnscheduledParams(t, "vm-unsched")
	id, err := st.CreateUnscheduledVM(ctx, p)
	if err != nil {
		t.Fatalf("CreateUnscheduledVM: %v", err)
	}
	if id != p.ID {
		t.Errorf("CreateUnscheduledVM id = %v, want %v", id, p.ID)
	}

	vm, err := st.VMByID(ctx, id)
	if err != nil {
		t.Fatalf("VMByID: %v", err)
	}
	if vm.SchedulingStatus != store.VMSchedulingUnscheduled {
		t.Errorf("SchedulingStatus = %q, want %q", vm.SchedulingStatus, store.VMSchedulingUnscheduled)
	}
	if vm.SchedulingReason == nil || *vm.SchedulingReason != store.SchedReasonPendingSchedule {
		t.Errorf("SchedulingReason = %v, want %q", vm.SchedulingReason, store.SchedReasonPendingSchedule)
	}
	if vm.PinnedNodeID != nil {
		t.Errorf("PinnedNodeID = %v, want nil", vm.PinnedNodeID)
	}

	// No disk / nic / task rows yet.
	disks, err := st.ListVMDisksByVM(ctx, id)
	if err != nil {
		t.Fatalf("ListVMDisksByVM: %v", err)
	}
	if len(disks) != 0 {
		t.Errorf("disks = %d, want 0", len(disks))
	}

	// Appears in the unscheduled list.
	list, err := st.ListUnscheduledVMs(ctx, 100)
	if err != nil {
		t.Fatalf("ListUnscheduledVMs: %v", err)
	}
	if len(list) != 1 || list[0].ID != id {
		t.Errorf("ListUnscheduledVMs = %v, want [%v]", list, id)
	}

	// Name uniqueness still enforced.
	dup := mkUnscheduledParams(t, "vm-unsched")
	if _, err := st.CreateUnscheduledVM(ctx, dup); err == nil {
		t.Error("CreateUnscheduledVM(dup name) = nil, want ErrVMNameInUse")
	}
}

func TestListUnscheduledVMs_LimitCap(t *testing.T) {
	st, _ := etcdstore.FreshStore(t)
	ctx := context.Background()

	for _, name := range []string{"vm-a", "vm-b", "vm-c"} {
		if _, err := st.CreateUnscheduledVM(ctx, mkUnscheduledParams(t, name)); err != nil {
			t.Fatalf("CreateUnscheduledVM(%s): %v", name, err)
		}
	}

	capped, err := st.ListUnscheduledVMs(ctx, 2)
	if err != nil {
		t.Fatalf("ListUnscheduledVMs(limit=2): %v", err)
	}
	if len(capped) != 2 {
		t.Errorf("ListUnscheduledVMs(limit=2) returned %d, want 2", len(capped))
	}

	all, err := st.ListUnscheduledVMs(ctx, 0)
	if err != nil {
		t.Fatalf("ListUnscheduledVMs(limit=0): %v", err)
	}
	if len(all) != 3 {
		t.Errorf("ListUnscheduledVMs(limit=0) returned %d, want 3 (no cap)", len(all))
	}
}

func TestUpdateVMSchedulingReason(t *testing.T) {
	st, _ := etcdstore.FreshStore(t)
	ctx := context.Background()

	id, err := st.CreateUnscheduledVM(ctx, mkUnscheduledParams(t, "vm-reason"))
	if err != nil {
		t.Fatalf("CreateUnscheduledVM: %v", err)
	}

	if err := st.UpdateVMSchedulingReason(ctx, id, store.SchedReasonPoolNotReady, `pool "default" not ready`, nil); err != nil {
		t.Fatalf("UpdateVMSchedulingReason: %v", err)
	}
	vm, err := st.VMByID(ctx, id)
	if err != nil {
		t.Fatalf("VMByID: %v", err)
	}
	if vm.SchedulingReason == nil || *vm.SchedulingReason != store.SchedReasonPoolNotReady {
		t.Errorf("reason = %v, want %q", vm.SchedulingReason, store.SchedReasonPoolNotReady)
	}
	if vm.SchedulingMessage == nil || *vm.SchedulingMessage != `pool "default" not ready` {
		t.Errorf("message = %v, want %q", vm.SchedulingMessage, `pool "default" not ready`)
	}
	if vm.LastScheduleAttemptAt == nil {
		t.Error("LastScheduleAttemptAt = nil, want set")
	}

	// Missing VM -> ErrNotFound (defensive).
	if err := st.UpdateVMSchedulingReason(ctx, uuid.New(), store.SchedReasonPoolNotReady, "x", nil); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UpdateVMSchedulingReason(missing) err = %v, want ErrNotFound", err)
	}
}

func TestBindScheduledVM(t *testing.T) {
	st, _ := etcdstore.FreshStore(t)
	ctx := context.Background()

	// Seed a ready node + pool the plan can place on.
	nodeID, poolID, poolName := schedulingFixture(t, st)

	vmID, err := st.CreateUnscheduledVM(ctx, mkUnscheduledParams(t, "vm-bind"))
	if err != nil {
		t.Fatalf("CreateUnscheduledVM: %v", err)
	}

	taskID := uuid.New()
	err = st.BindScheduledVM(ctx, vmID, func(pr store.PlacementReader) (store.VMBindWrites, error) {
		// In the real loop the worker runs scheduler.SchedulePlacement here; for
		// the store-level test we drive the placement reader, then bind directly
		// to the seeded node/pool.
		pools, perr := pr.ListEligiblePoolsByName(ctx, poolName)
		if perr != nil {
			return store.VMBindWrites{}, perr
		}
		if len(pools) == 0 {
			return store.VMBindWrites{}, fmt.Errorf("no eligible pool")
		}
		return store.VMBindWrites{
			PinnedNodeID: nodeID,
			Disk: store.CreateVMDiskParams{
				VmID: vmID, StoragePoolID: poolID, DeviceOrder: 0,
				Bus: store.DiskBusVirtio, SizeGib: 0, SourceKind: "image",
				Format: store.ImageFormatQcow2, CacheMode: store.DiskCacheModeWriteback,
				Discard: store.DiskDiscardUnmap,
			},
			Task: store.CreateTaskParams{
				ID: taskID, Type: "vm.create", Status: store.TaskStatusPending,
				ResourceType: "vm", ResourceID: &vmID, Args: []byte(`{}`), MaxAttempts: 25,
			},
			Job: stubJobArgs{},
		}, nil
	})
	if err != nil {
		t.Fatalf("BindScheduledVM: %v", err)
	}

	vm, err := st.VMByID(ctx, vmID)
	if err != nil {
		t.Fatalf("VMByID: %v", err)
	}
	if vm.SchedulingStatus != store.VMSchedulingScheduled {
		t.Errorf("status = %q, want scheduled", vm.SchedulingStatus)
	}
	if vm.PinnedNodeID == nil || *vm.PinnedNodeID != nodeID {
		t.Errorf("PinnedNodeID = %v, want %v", vm.PinnedNodeID, nodeID)
	}
	if vm.SchedulingReason != nil {
		t.Errorf("SchedulingReason = %v, want nil", vm.SchedulingReason)
	}

	disks, err := st.ListVMDisksByVM(ctx, vmID)
	if err != nil {
		t.Fatalf("ListVMDisksByVM: %v", err)
	}
	if len(disks) != 1 {
		t.Errorf("disks = %d, want 1", len(disks))
	}
	if _, err := st.TaskByID(ctx, taskID); err != nil {
		t.Errorf("TaskByID(bind task) err = %v, want nil", err)
	}

	// Unscheduled index dropped.
	list, err := st.ListUnscheduledVMs(ctx, 100)
	if err != nil {
		t.Fatalf("ListUnscheduledVMs: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("unscheduled after bind = %d, want 0", len(list))
	}

	// Double-bind guard: re-binding a now-scheduled VM loses the ModRevision CAS
	// and returns ErrVMNotUnscheduled.
	err = st.BindScheduledVM(ctx, vmID, func(store.PlacementReader) (store.VMBindWrites, error) {
		return store.VMBindWrites{
			PinnedNodeID: nodeID,
			Disk: store.CreateVMDiskParams{
				VmID: vmID, StoragePoolID: poolID, DeviceOrder: 0,
				Bus: store.DiskBusVirtio, SizeGib: 0, SourceKind: "image",
				Format: store.ImageFormatQcow2, CacheMode: store.DiskCacheModeWriteback,
				Discard: store.DiskDiscardUnmap,
			},
			Task: store.CreateTaskParams{
				ID: uuid.New(), Type: "vm.create", Status: store.TaskStatusPending,
				ResourceType: "vm", ResourceID: &vmID, Args: []byte(`{}`), MaxAttempts: 25,
			},
			Job: stubJobArgs{},
		}, nil
	})
	if !errors.Is(err, store.ErrVMNotUnscheduled) {
		t.Errorf("re-bind err = %v, want ErrVMNotUnscheduled", err)
	}

	// Task-3 deferred guard: a stale scheduling reason recorded after the bind
	// must NOT clobber the bound state (UpdateVMSchedulingReason is a no-op once
	// the VM is scheduled).
	if err := st.UpdateVMSchedulingReason(ctx, vmID, store.SchedReasonPoolNotReady, "stale", nil); err != nil {
		t.Fatalf("UpdateVMSchedulingReason(stale): %v", err)
	}
	vm2, err := st.VMByID(ctx, vmID)
	if err != nil {
		t.Fatalf("VMByID after stale reason: %v", err)
	}
	if vm2.SchedulingStatus != store.VMSchedulingScheduled {
		t.Errorf("status after stale reason = %q, want scheduled", vm2.SchedulingStatus)
	}
	if vm2.SchedulingReason != nil {
		t.Errorf("SchedulingReason after stale reason = %v, want nil", vm2.SchedulingReason)
	}
}

func TestDeleteUnscheduledVM(t *testing.T) {
	st, cli := etcdstore.FreshStore(t)
	ctx := context.Background()

	id, err := st.CreateUnscheduledVM(ctx, mkUnscheduledParams(t, "vm-del"))
	if err != nil {
		t.Fatalf("CreateUnscheduledVM: %v", err)
	}

	if err := st.DeleteUnscheduledVM(ctx, id); err != nil {
		t.Fatalf("DeleteUnscheduledVM: %v", err)
	}

	// The VM row is gone (hard delete - no soft-delete tombstone).
	if _, err := st.VMByID(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("VMByID after delete err = %v, want ErrNotFound", err)
	}

	// Dropped from the unscheduled index.
	list, err := st.ListUnscheduledVMs(ctx, 100)
	if err != nil {
		t.Fatalf("ListUnscheduledVMs: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("unscheduled after delete = %d, want 0", len(list))
	}

	// Pin the raw index drop directly: ListUnscheduledVMs skips rows whose VM
	// is gone, so it would mask a leaked index key. Range the prefix to prove
	// the entry was physically removed, not just orphaned.
	if items, err := cli.Range(ctx, etcd.Key("index", "vms", "unscheduled")+"/"); err != nil {
		t.Fatalf("range unscheduled index: %v", err)
	} else if len(items) != 0 {
		t.Errorf("unscheduled index keys after delete = %d, want 0", len(items))
	}

	// The name is reusable immediately (the name guard was dropped).
	reID, err := st.CreateUnscheduledVM(ctx, mkUnscheduledParams(t, "vm-del"))
	if err != nil {
		t.Fatalf("CreateUnscheduledVM(reuse name): %v", err)
	}
	if reID == id {
		t.Errorf("reused id = %v, want a fresh id (not %v)", reID, id)
	}

	// Re-deleting the now-missing original id -> ErrNotFound.
	if err := st.DeleteUnscheduledVM(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DeleteUnscheduledVM(missing) err = %v, want ErrNotFound", err)
	}
}

func TestDeleteUnscheduledVM_RejectsScheduled(t *testing.T) {
	st, _ := etcdstore.FreshStore(t)
	ctx := context.Background()

	nodeID, poolID, poolName := schedulingFixture(t, st)
	vmID, err := st.CreateUnscheduledVM(ctx, mkUnscheduledParams(t, "vm-del-bound"))
	if err != nil {
		t.Fatalf("CreateUnscheduledVM: %v", err)
	}

	taskID := uuid.New()
	if err := st.BindScheduledVM(ctx, vmID, func(pr store.PlacementReader) (store.VMBindWrites, error) {
		pools, perr := pr.ListEligiblePoolsByName(ctx, poolName)
		if perr != nil {
			return store.VMBindWrites{}, perr
		}
		if len(pools) == 0 {
			return store.VMBindWrites{}, fmt.Errorf("no eligible pool")
		}
		return store.VMBindWrites{
			PinnedNodeID: nodeID,
			Disk: store.CreateVMDiskParams{
				VmID: vmID, StoragePoolID: poolID, DeviceOrder: 0,
				Bus: store.DiskBusVirtio, SizeGib: 0, SourceKind: "image",
				Format: store.ImageFormatQcow2, CacheMode: store.DiskCacheModeWriteback,
				Discard: store.DiskDiscardUnmap,
			},
			Task: store.CreateTaskParams{
				ID: taskID, Type: "vm.create", Status: store.TaskStatusPending,
				ResourceType: "vm", ResourceID: &vmID, Args: []byte(`{}`), MaxAttempts: 25,
			},
			Job: stubJobArgs{},
		}, nil
	}); err != nil {
		t.Fatalf("BindScheduledVM: %v", err)
	}

	// A scheduled VM is NOT hard-deletable through this path: it reached the
	// async pipeline (disk/task/job), so the caller must fall back to the agent
	// delete. The VM row must survive.
	if err := st.DeleteUnscheduledVM(ctx, vmID); !errors.Is(err, store.ErrVMNotUnscheduled) {
		t.Errorf("DeleteUnscheduledVM(scheduled) err = %v, want ErrVMNotUnscheduled", err)
	}
	if _, err := st.VMByID(ctx, vmID); err != nil {
		t.Errorf("VMByID after rejected delete err = %v, want nil (row preserved)", err)
	}
}

// TestBindScheduledVMRefusesADeletedVM pins the guard that makes the
// unscheduled delete's soft-delete safe. BindScheduledVM used to read the raw
// row and treat "the key is absent" as the only delete signal, so once the
// delete stopped removing the key, a scheduler tick racing a user's delete would
// bind a VM every reader already 404s.
//
// The damage would be unreclaimable rather than merely untidy: the bind commits
// a boot disk, a NIC with its MAC guard and CP-IPAM IPv4 reservation, a pinned
// index and a create task, and none of it can be undone afterwards - `vm delete`
// 404s, so ProjectVMDeleteSuccess never runs, and the leaked per-network NIC
// index wedges DeleteNetwork forever.
//
// The real order matters: the delete lands BEFORE the bind's read, so the
// ModRevision CAS cannot help - it compares against the post-delete revision and
// succeeds. Only a DeletedAt check refuses this.
func TestBindScheduledVMRefusesADeletedVM(t *testing.T) {
	st, _ := etcdstore.FreshStore(t)
	ctx := context.Background()

	nodeID, poolID, poolName := schedulingFixture(t, st)
	vmID, err := st.CreateUnscheduledVM(ctx, mkUnscheduledParams(t, "vm-bind-deleted"))
	if err != nil {
		t.Fatalf("CreateUnscheduledVM: %v", err)
	}
	if err := st.DeleteUnscheduledVM(ctx, vmID); err != nil {
		t.Fatalf("DeleteUnscheduledVM: %v", err)
	}

	planned := false
	taskID := uuid.New()
	err = st.BindScheduledVM(ctx, vmID, func(pr store.PlacementReader) (store.VMBindWrites, error) {
		planned = true
		pools, perr := pr.ListEligiblePoolsByName(ctx, poolName)
		if perr != nil {
			return store.VMBindWrites{}, perr
		}
		if len(pools) == 0 {
			return store.VMBindWrites{}, fmt.Errorf("no eligible pool")
		}
		return store.VMBindWrites{
			PinnedNodeID: nodeID,
			Disk: store.CreateVMDiskParams{
				VmID: vmID, StoragePoolID: poolID, DeviceOrder: 0,
				Bus: store.DiskBusVirtio, SizeGib: 0, SourceKind: "image",
				Format: store.ImageFormatQcow2, CacheMode: store.DiskCacheModeWriteback,
				Discard: store.DiskDiscardUnmap,
			},
			Task: store.CreateTaskParams{
				ID: taskID, Type: "vm.create", Status: store.TaskStatusPending,
				ResourceType: "vm", ResourceID: &vmID, Args: []byte(`{}`), MaxAttempts: 25,
			},
			Job: stubJobArgs{},
		}, nil
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("BindScheduledVM(deleted vm) = %v, want store.ErrNotFound (the scheduler's clean-skip sentinel)", err)
	}
	if planned {
		t.Errorf("plan() ran for a deleted VM; the guard must refuse before placement")
	}

	// Nothing was committed on the VM's behalf.
	disks, err := st.ListVMDisksByVM(ctx, vmID)
	if err != nil {
		t.Fatalf("ListVMDisksByVM: %v", err)
	}
	if len(disks) != 0 {
		t.Errorf("disks after a refused bind = %d, want 0", len(disks))
	}
	if _, err := st.TaskByID(ctx, taskID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("TaskByID(create) after a refused bind = %v, want ErrNotFound", err)
	}
}
