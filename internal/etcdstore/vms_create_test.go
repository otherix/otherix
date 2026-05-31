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

	vmshandlers "github.com/otherix/otherix/internal/api/handlers/vms"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// The etcd store now satisfies EVERY handler Store interface, the final one
// being vms (CreateScheduledVM + the placement reader).
var _ vmshandlers.Store = (*etcdstore.Store)(nil)

// schedulingFixture seeds a ready node, a pool on it, and a template, returning
// the node and pool ids for placement.
func schedulingFixture(t *testing.T, s *etcdstore.Store) (nodeID, poolID, templateID uuid.UUID, poolName string) {
	t.Helper()
	ctx := context.Background()
	np := nodeParams(uniqueNodeName("sched"))
	if _, err := s.CreateNode(ctx, np); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	// Mark the node ready (CreateNode lands it pending).
	if _, err := s.UncordonNode(ctx, np.ID); err != nil {
		t.Fatalf("UncordonNode: %v", err)
	}
	pp := poolParams(np.ID, uniquePoolName("sched"))
	if _, err := s.CreateStoragePool(ctx, pp); err != nil {
		t.Fatalf("CreateStoragePool: %v", err)
	}
	tp := tplParams(uniqueTplName("sched"), uuid.New())
	if _, err := s.CreateTemplate(ctx, tp); err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	return np.ID, pp.ID, tp.ID, pp.Name
}

func vmCreateWrites(t *testing.T, name string, owner, nodeID, poolID, templateID uuid.UUID) store.VMCreateWrites {
	t.Helper()
	taskID := uuid.New()
	vmID := uuid.New()
	return store.VMCreateWrites{
		VM: store.CreateVMParams{
			ID: vmID, OwnerID: owner, Name: name, Architecture: store.CpuArchAmd64,
			CpuCores: 2, MemoryMib: 2048, TemplateID: &templateID, PinnedNodeID: &nodeID,
		},
		Disk: store.CreateVMDiskParams{
			VmID: vmID, StoragePoolID: poolID, DeviceOrder: 0, Bus: store.DiskBusVirtio,
			SizeGib: 20, SourceKind: "template", SourceTemplateID: &templateID, Format: store.ImageFormatQcow2,
			CacheMode: store.DiskCacheModeNone, Discard: store.DiskDiscardUnmap,
		},
		Task: store.CreateTaskParams{
			ID: taskID, Type: "vm.create", Status: store.TaskStatusPending, ResourceType: "vm", ResourceID: &vmID,
			Args: []byte(`{}`), MaxAttempts: 3, CreatedBy: &owner,
		},
		Job: testJobArgs{Foo: "vm-create"},
	}
}

func TestCreateScheduledVMHappyPath(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	nodeID, poolID, templateID, poolName := schedulingFixture(t, s)
	owner := uuid.New()
	name := "vm-" + uuid.NewString()[:8]

	var sawEligible int
	taskID, err := s.CreateScheduledVM(ctx, func(pr store.PlacementReader) (store.VMCreateWrites, error) {
		if err := pr.AcquirePlacementLock(ctx, 1); err != nil {
			return store.VMCreateWrites{}, err
		}
		eligible, err := pr.ListEligiblePoolsByName(ctx, poolName)
		if err != nil {
			return store.VMCreateWrites{}, err
		}
		sawEligible = len(eligible)
		return vmCreateWrites(t, name, owner, nodeID, poolID, templateID), nil
	})
	if err != nil {
		t.Fatalf("CreateScheduledVM: %v", err)
	}
	if sawEligible != 1 {
		t.Errorf("eligible pools = %d, want 1 (ready node)", sawEligible)
	}
	// Task is pending with a job ref.
	task, err := s.TaskByID(ctx, taskID)
	if err != nil || task.Status != store.TaskStatusPending || task.RiverJobID == nil {
		t.Fatalf("task = (%+v, %v)", task, err)
	}
	// VM resolves by name + has a disk + counts toward the node's pinned load.
	vm, err := s.VMByName(ctx, name)
	if err != nil {
		t.Fatalf("VMByName: %v", err)
	}
	if vm.DesiredPhase != store.VmDesiredPhaseRunning || vm.Generation != 1 {
		t.Errorf("vm = %+v, want running + generation 1", vm)
	}
	disks, err := s.ListVMDisksByVM(ctx, vm.ID)
	if err != nil || len(disks) != 1 || disks[0].StoragePoolID != poolID {
		t.Errorf("disks = (%v, %v), want one on pool %v", disks, err, poolID)
	}
	cnt, err := placementCount(ctx, s, nodeID)
	if err != nil || cnt != 1 {
		t.Errorf("CountRunningVMsByNode = (%d, %v), want 1", cnt, err)
	}
}

func TestCreateScheduledVMDuplicateNameAndPlanError(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	nodeID, poolID, templateID, _ := schedulingFixture(t, s)
	owner := uuid.New()
	name := "dup-" + uuid.NewString()[:8]

	if _, err := s.CreateScheduledVM(ctx, func(store.PlacementReader) (store.VMCreateWrites, error) {
		return vmCreateWrites(t, name, owner, nodeID, poolID, templateID), nil
	}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	// Same name collides.
	if _, err := s.CreateScheduledVM(ctx, func(store.PlacementReader) (store.VMCreateWrites, error) {
		return vmCreateWrites(t, name, owner, nodeID, poolID, templateID), nil
	}); !errors.Is(err, store.ErrVMNameInUse) {
		t.Errorf("duplicate name = %v, want store.ErrVMNameInUse", err)
	}
	// Plan error propagates verbatim, nothing persisted.
	sentinel := errors.New("no capacity")
	if _, err := s.CreateScheduledVM(ctx, func(store.PlacementReader) (store.VMCreateWrites, error) {
		return store.VMCreateWrites{}, sentinel
	}); !errors.Is(err, sentinel) {
		t.Errorf("plan error = %v, want propagated sentinel", err)
	}
}

func TestPlacementReaderExcludesCordonedNode(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	nodeID, _, _, poolName := schedulingFixture(t, s)
	// Cordon the node -> no eligible pools, but it shows as a (non-pressured)
	// candidate is still excluded since cordoned fails the schedulable base.
	if _, err := s.CordonNode(ctx, nodeID); err != nil {
		t.Fatalf("CordonNode: %v", err)
	}
	_, err := s.CreateScheduledVM(ctx, func(pr store.PlacementReader) (store.VMCreateWrites, error) {
		eligible, err := pr.ListEligiblePoolsByName(ctx, poolName)
		if err != nil {
			return store.VMCreateWrites{}, err
		}
		if len(eligible) != 0 {
			t.Errorf("eligible on cordoned node = %d, want 0", len(eligible))
		}
		return store.VMCreateWrites{}, errors.New("stop")
	})
	if err == nil {
		t.Errorf("expected the plan stop error")
	}
}

// placementCount exercises CountRunningVMsByNode through a throwaway plan.
func placementCount(ctx context.Context, s *etcdstore.Store, nodeID uuid.UUID) (int64, error) {
	var n int64
	stop := errors.New("stop")
	_, err := s.CreateScheduledVM(ctx, func(pr store.PlacementReader) (store.VMCreateWrites, error) {
		var e error
		n, e = pr.CountRunningVMsByNode(ctx, &nodeID)
		if e != nil {
			return store.VMCreateWrites{}, e
		}
		return store.VMCreateWrites{}, stop
	})
	if errors.Is(err, stop) {
		return n, nil
	}
	return n, err
}
