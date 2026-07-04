// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	firmwareshandlers "github.com/otherix/otherix/internal/api/handlers/firmwares"
	nodeshandlers "github.com/otherix/otherix/internal/api/handlers/nodes"
	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// With all six resolver.Querier lookups implemented, the etcd store satisfies
// the firmwares and nodes handler contracts (neither needs the queue seam).
var (
	_ firmwareshandlers.Store = (*etcdstore.Store)(nil)
	_ nodeshandlers.Store     = (*etcdstore.Store)(nil)
)

// seedVM writes a vms row + its name guard so VMByName resolves (the production
// write path lands with the queue slice).
func seedVM(t *testing.T, cli *etcd.Client, v store.VM) {
	t.Helper()
	ctx := context.Background()
	if err := cli.PutJSON(ctx, etcd.Key("vms", v.ID.String()), v); err != nil {
		t.Fatalf("seed vm: %v", err)
	}
	if v.DeletedAt == nil {
		if err := cli.Put(ctx, etcd.Key("uniq", "vms", "name", lower(v.Name)), []byte(v.ID.String())); err != nil {
			t.Fatalf("seed vm guard: %v", err)
		}
		// Mirror CreateVM's owner index so index-authoritative lookups
		// (ListVMsByOwner) resolve the seed. A soft-deleted seed gets no
		// index entry, matching production where delete removes it.
		if err := cli.Put(ctx, etcd.Key("index", "vms", "owner", v.OwnerID.String(), v.ID.String()), []byte(v.ID.String())); err != nil {
			t.Fatalf("seed vm owner index: %v", err)
		}
	}
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

func vmRow(name string) store.VM {
	return store.VM{
		ID: uuid.New(), OwnerID: uuid.New(), Name: name, DesiredPhase: store.VmDesiredPhaseRunning,
		Architecture: store.CpuArchAmd64, CpuCores: 2, MemoryMib: 2048, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
}

func TestVMByNameAndByID(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	v := vmRow("web-01")
	seedVM(t, cli, v)
	byName, err := s.VMByName(ctx, "WEB-01")
	if err != nil || byName.ID != v.ID {
		t.Fatalf("VMByName(upper) = (%+v, %v)", byName, err)
	}
	byID, err := s.VMByID(ctx, v.ID)
	if err != nil || byID.Name != "web-01" {
		t.Fatalf("VMByID = (%+v, %v)", byID, err)
	}
	if _, err := s.VMByName(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("VMByName(absent) = %v, want store.ErrNotFound", err)
	}
}

func TestVMRuntimeByIDAndUpdatePhase(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	vmID := uuid.New()
	// No runtime row yet.
	if _, err := s.VMRuntimeByID(ctx, vmID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("VMRuntimeByID(absent) = %v, want store.ErrNotFound", err)
	}
	// Phase update on a missing runtime is a no-op.
	if err := s.UpdateVMRuntimePhase(ctx, store.UpdateVMRuntimePhaseParams{VmID: vmID, Phase: store.VmPhaseRunning}); err != nil {
		t.Errorf("UpdateVMRuntimePhase(absent) = %v, want nil", err)
	}
	// Seed a runtime row, then update.
	rt := store.VMRuntime{VmID: vmID, Phase: store.VmPhasePending}
	if err := cli.PutJSON(ctx, etcd.Key("vm_runtime", vmID.String()), rt); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	if err := s.UpdateVMRuntimePhase(ctx, store.UpdateVMRuntimePhaseParams{VmID: vmID, Phase: store.VmPhasePaused}); err != nil {
		t.Fatalf("UpdateVMRuntimePhase: %v", err)
	}
	got, err := s.VMRuntimeByID(ctx, vmID)
	if err != nil {
		t.Fatalf("VMRuntimeByID: %v", err)
	}
	if got.Phase != store.VmPhasePaused || got.LastObservedAt == nil {
		t.Errorf("runtime = %+v, want paused + last_observed_at", got)
	}
}

func TestListVMDisksByVMOrdering(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	vmID := uuid.New()
	for _, order := range []int32{2, 0, 1} {
		d := store.VMDisk{ID: uuid.New(), VmID: vmID, StoragePoolID: uuid.New(), DeviceOrder: order, SizeGib: 10, CreatedAt: time.Now().UTC()}
		if err := cli.PutJSON(ctx, etcd.Key("vm_disks", d.ID.String()), d); err != nil {
			t.Fatalf("seed disk: %v", err)
		}
		if err := cli.Put(ctx, etcd.Key("index", "vm_disks", "vm", vmID.String(), d.ID.String()), []byte(d.ID.String())); err != nil {
			t.Fatalf("seed disk index: %v", err)
		}
	}
	disks, err := s.ListVMDisksByVM(ctx, vmID)
	if err != nil {
		t.Fatalf("ListVMDisksByVM: %v", err)
	}
	if len(disks) != 3 || disks[0].DeviceOrder != 0 || disks[1].DeviceOrder != 1 || disks[2].DeviceOrder != 2 {
		t.Errorf("disks order = %+v, want device_order 0,1,2", disks)
	}
}

func TestListVMsFiltersAndOrdering(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	var ids []uuid.UUID
	for i := 0; i < 3; i++ {
		v := vmRow(uniqueNodeName("vm"))
		seedVM(t, cli, v)
		ids = append(ids, v.ID)
		time.Sleep(2 * time.Millisecond)
	}
	all, err := s.ListVMs(ctx, store.ListVMsParams{LimitCount: 200})
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if len(all) < 3 {
		t.Fatalf("ListVMs len = %d, want >= 3", len(all))
	}
	// Descending (created_at, id).
	for i := 1; i < len(all); i++ {
		if all[i].CreatedAt.After(all[i-1].CreatedAt) {
			t.Errorf("not descending at %d", i)
		}
	}
	// Node filter via a runtime row on a node.
	node := uuid.New()
	target := ids[0]
	rt := store.VMRuntime{VmID: target, CurrentNodeID: &node, Phase: store.VmPhaseRunning}
	if err := cli.PutJSON(ctx, etcd.Key("vm_runtime", target.String()), rt); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	onNode, err := s.ListVMs(ctx, store.ListVMsParams{NodeIDFilter: &node, LimitCount: 200})
	if err != nil {
		t.Fatalf("ListVMs(node): %v", err)
	}
	if len(onNode) != 1 || onNode[0].ID != target {
		t.Errorf("ListVMs(node) = %v, want [%v]", onNode, target)
	}
}
