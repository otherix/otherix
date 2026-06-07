// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration

package etcdstore_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

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
