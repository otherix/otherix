// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// TestVMDeleteProjectionStaysUnderTxnBudget guards the forward constraint that
// ProjectVMDeleteSuccess commits the entire VM soft-delete (the VM row, its
// disks, NICs, secondary indexes, the runtime row + its by-node index, and the
// task finalize) in a SINGLE etcd transaction. etcd's default --max-txn-ops is
// 128. The current create model is single-root-disk + single-NIC, so a delete
// is well under that. This test seeds a VM the normal way (one disk, one NIC, on
// a node, backed by a template) and asserts the delete projection's op count
// stays well under the budget. It fails only if a future change (for example a
// multi-disk/NIC hotplug-attach path) roughly quintuples the per-VM op count
// without adding a per-VM op-budget cap or chunking the delete. See
// docs/architecture.md ("Delete projection op budget").
func TestVMDeleteProjectionStaysUnderTxnBudget(t *testing.T) {
	s := startInternalStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	ownerID := uuid.New()
	nodeID := uuid.New()
	poolID := uuid.New()
	templateID := uuid.New()
	firmwareID := uuid.New()
	networkID := uuid.New()
	vmID := uuid.New()
	taskID := uuid.New()

	// Template row, so the templated delete path appends the non-idempotent
	// derived_vm_count decrement (one extra put on top of the base ops).
	tmpl := store.Template{ID: templateID, OwnerID: ownerID, Name: "tmpl-" + uuid.NewString()[:8], CreatedAt: now, UpdatedAt: now}
	if err := s.c.PutJSON(ctx, templateKey(templateID), tmpl); err != nil {
		t.Fatalf("seed template: %v", err)
	}

	// VM row with template + firmware + pinned-node set, so every vmIndexDeleteOps
	// branch fires (owner, template, firmware, pinned-node). This is the widest
	// index set a single VM carries today.
	vm := store.VM{
		ID:           vmID,
		OwnerID:      ownerID,
		Name:         "vm-" + uuid.NewString()[:8],
		DesiredPhase: store.VmDesiredPhaseRunning,
		TemplateID:   &templateID,
		FirmwareID:   &firmwareID,
		PinnedNodeID: &nodeID,
		Architecture: store.CpuArchAmd64,
		CpuCores:     2,
		MemoryMib:    2048,
		Generation:   1,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.c.PutJSON(ctx, vmKey(vmID), vm); err != nil {
		t.Fatalf("seed vm: %v", err)
	}
	if err := s.c.Put(ctx, vmNameGuard(vm.Name), []byte(vmID.String())); err != nil {
		t.Fatalf("seed name guard: %v", err)
	}
	if _, err := s.c.Raw().Txn(ctx).Then(vmIndexOps(vm)...).Commit(); err != nil {
		t.Fatalf("seed vm indexes: %v", err)
	}

	// One disk, with its vm + pool indexes (the normal single-root-disk create).
	disk := store.VMDisk{
		ID: uuid.New(), VmID: vmID, StoragePoolID: poolID, DeviceOrder: 0,
		Bus: store.DiskBusVirtio, SizeGib: 20, SourceKind: "template", SourceTemplateID: &templateID,
		Format: store.ImageFormatQcow2, CacheMode: store.DiskCacheModeNone, Discard: store.DiskDiscardUnmap,
		Generation: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.c.PutJSON(ctx, vmDiskKey(disk.ID), disk); err != nil {
		t.Fatalf("seed disk: %v", err)
	}
	if _, err := s.c.Raw().Txn(ctx).Then(vmDiskIndexOps(disk)...).Commit(); err != nil {
		t.Fatalf("seed disk indexes: %v", err)
	}

	// One NIC, via the real create-side op builder (row + vm index + network
	// index + MAC guard), so the delete-side mirror finds it.
	nic := store.VMNic{
		ID: uuid.New(), VmID: vmID, NetworkID: networkID, DeviceOrder: 0,
		Model: store.NicModelVirtio, MacAddress: net.HardwareAddr{0x52, 0x54, 0x00, 0x12, 0x34, 0x56},
		Generation: 1, CreatedAt: now, UpdatedAt: now,
	}
	nicOps, err := vmNicCreateOps(nic)
	if err != nil {
		t.Fatalf("build nic create ops: %v", err)
	}
	if _, err := s.c.Raw().Txn(ctx).Then(nicOps...).Commit(); err != nil {
		t.Fatalf("seed nic: %v", err)
	}

	// Runtime row placed on the node, plus the vm_runtime-by-node index the
	// heartbeat writes, so the delete projection emits that index removal too.
	rt := store.VMRuntime{VmID: vmID, CurrentNodeID: &nodeID, Phase: store.VmPhaseRunning, LastObservedAt: &now, UpdatedAt: now}
	if err := s.c.PutJSON(ctx, vmRuntimeKey(vmID), rt); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	if err := s.c.Put(ctx, etcd.Key("index", "vm_runtime", "node", nodeID.String(), vmID.String()), []byte(vmID.String())); err != nil {
		t.Fatalf("seed runtime-node index: %v", err)
	}

	// Build the exact ops the projection would commit: the base soft-delete ops
	// plus the templated derived_vm_count put. vmDeleteCommitInputs mirrors the
	// real assembly inside ProjectVMDeleteSuccess, so opCount is precise rather
	// than an estimate.
	base, err := s.vmDeleteBaseOps(ctx, vm, now, []byte(`{}`), taskID)
	if err != nil {
		t.Fatalf("vmDeleteBaseOps: %v", err)
	}
	_, ops, err := s.vmDeleteCommitInputs(ctx, vm, base)
	if err != nil {
		t.Fatalf("vmDeleteCommitInputs: %v", err)
	}
	opCount := len(ops)

	// Sanity: a templated commit is exactly the base ops plus one (the
	// derived_vm_count put). If that ever stops holding, the count below is no
	// longer the faithful total and the assembly drifted.
	if opCount != len(base)+1 {
		t.Errorf("committed ops = %d, base = %d; templated commit should be base+1", opCount, len(base))
	}

	t.Logf("single-disk/single-NIC vm delete projection = %d ops (base %d + template count 1); etcd --max-txn-ops default is 128", opCount, len(base))

	if opCount > 100 {
		t.Errorf("vm delete projection = %d ops, want <= 100; a multi-disk/NIC feature must add a per-VM op-budget cap or chunk the delete (see docs/architecture.md)", opCount)
	}

	// The ops must actually commit (proves the seeded keys are real and the
	// builder emits a well-formed transaction, not just a count).
	if _, err := s.c.Raw().Txn(ctx).Then(ops...).Commit(); err != nil {
		t.Fatalf("commit delete projection ops: %v", err)
	}
}
