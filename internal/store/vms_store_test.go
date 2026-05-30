// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// The tests in this file exercise the VM domain methods on *Store: the
// VMRuntimeByID not-found translation and the placement-locked
// CreateScheduledVM orchestration (happy path, name-conflict
// translation, plan-error propagation). The real scheduler is exercised
// end-to-end by the vms create e2e; here the plan callback returns
// pre-built writes so the test isolates the store's transaction +
// translation responsibilities.

// scheduledWrites builds a minimal valid VMCreateWrites for owner /
// template / pool / node with the given VM name and ids.
func scheduledWrites(vmID, taskID uuid.UUID, owner, tpl, pool, node uuid.UUID, name string) store.VMCreateWrites {
	tid := tpl
	nid := node
	createdBy := owner
	resID := vmID
	return store.VMCreateWrites{
		VM: store.CreateVMParams{
			ID:           vmID,
			OwnerID:      owner,
			Name:         name,
			Description:  "",
			TemplateID:   &tid,
			Architecture: store.CpuArchAmd64,
			CpuCores:     2,
			MemoryMib:    2048,
			CPUModel:     "host",
			MachineType:  "q35",
			FirmwareID:   nil,
			PinnedNodeID: &nid,
			Labels:       []byte(`{}`),
		},
		Disk: store.CreateVMDiskParams{
			VmID:             vmID,
			StoragePoolID:    pool,
			DeviceOrder:      0,
			Bus:              store.DiskBusVirtio,
			SizeGib:          20,
			SourceKind:       "template",
			SourceTemplateID: &tid,
			Format:           store.ImageFormatQcow2,
			ReadOnly:         false,
			CacheMode:        store.DiskCacheModeWriteback,
			Discard:          store.DiskDiscardUnmap,
		},
		Task: store.CreateTaskParams{
			ID:           taskID,
			Type:         "vm.create",
			Status:       store.TaskStatusPending,
			ResourceType: "vm",
			ResourceID:   &resID,
			Args:         []byte(`{}`),
			MaxAttempts:  25,
			CreatedBy:    &createdBy,
		},
		Job: importArgsStub{},
	}
}

func TestVMRuntimeByIDNotFound(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	if _, err := s.VMRuntimeByID(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("VMRuntimeByID(absent) err = %v, want store.ErrNotFound", err)
	}
}

func TestCreateScheduledVMHappyPath(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)
	s.SetQueueBinder(noopBinder{})

	owner := seedUser(t, ctx, s, "developer")
	node := seedNodeForPools(t, ctx, s)
	tpl := seedTemplateForImages(t, ctx, s, owner, "vm-happy")
	pool := seedPoolForImages(t, ctx, s, node, "vm-happy")

	vmID := uuid.New()
	taskID := uuid.New()
	writes := scheduledWrites(vmID, taskID, owner, tpl, pool, node, uniqueTemplateName("vm-happy"))

	got, err := s.CreateScheduledVM(ctx, func(store.PlacementReader) (store.VMCreateWrites, error) {
		return writes, nil
	})
	if err != nil {
		t.Fatalf("CreateScheduledVM: %v", err)
	}
	if got != taskID {
		t.Errorf("CreateScheduledVM returned task %v, want %v", got, taskID)
	}
	if _, err := s.Queries().GetVMByID(ctx, vmID); err != nil {
		t.Errorf("VM row not persisted: %v", err)
	}
	if _, err := s.TaskByID(ctx, taskID); err != nil {
		t.Errorf("task row not persisted: %v", err)
	}
}

func TestCreateScheduledVMNameConflict(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)
	s.SetQueueBinder(noopBinder{})

	owner := seedUser(t, ctx, s, "developer")
	node := seedNodeForPools(t, ctx, s)
	tpl := seedTemplateForImages(t, ctx, s, owner, "vm-dup")
	pool := seedPoolForImages(t, ctx, s, node, "vm-dup")

	name := uniqueTemplateName("vm-dup")
	first := scheduledWrites(uuid.New(), uuid.New(), owner, tpl, pool, node, name)
	if _, err := s.CreateScheduledVM(ctx, func(store.PlacementReader) (store.VMCreateWrites, error) {
		return first, nil
	}); err != nil {
		t.Fatalf("first CreateScheduledVM: %v", err)
	}

	second := scheduledWrites(uuid.New(), uuid.New(), owner, tpl, pool, node, name)
	_, err := s.CreateScheduledVM(ctx, func(store.PlacementReader) (store.VMCreateWrites, error) {
		return second, nil
	})
	if !errors.Is(err, store.ErrVMNameInUse) {
		t.Errorf("duplicate-name CreateScheduledVM err = %v, want store.ErrVMNameInUse", err)
	}
}

func TestCreateScheduledVMPlanErrorPropagates(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)
	s.SetQueueBinder(noopBinder{})

	sentinel := errors.New("no eligible nodes")
	_, err := s.CreateScheduledVM(ctx, func(store.PlacementReader) (store.VMCreateWrites, error) {
		return store.VMCreateWrites{}, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("plan-error CreateScheduledVM err = %v, want sentinel", err)
	}
}
