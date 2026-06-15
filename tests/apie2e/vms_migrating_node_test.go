// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// TestVMGet_MigratingPinsDisplayedNodeToSource locks the M2 UX rule: while a
// migration is in flight, GET /v1/vms/{name} must display the VM on its SOURCE
// node (where the guest actually runs until cutover), NOT on the target. The
// target agent overwrites vm_runtime.current_node_id to itself the moment it
// heartbeats the incoming VM - well before cutover - so observedNodeID would
// otherwise show the target early. resolveViewNames overrides the displayed
// node to the migration's source while a non-terminal migration targets the VM.
//
// It also confirms M1 still holds: status.phase reads "migrating".
func TestVMGet_MigratingPinsDisplayedNodeToSource(t *testing.T) {
	h := newE2E(t)
	opToken, opID := loginAs(t, h, auth.RoleOperator)
	ctx := context.Background()
	s := h.store

	source := createReadyNode(t, s, "source")
	target := createReadyNode(t, s, "target")

	poolID := uuid.New()
	poolName := "pool-" + uuid.NewString()[:8]
	if _, err := s.CreateStoragePool(ctx, store.CreateStoragePoolParams{
		ID: poolID, NodeID: source.ID, Name: poolName, Type: "local_dir",
		Path: "/var/lib/otherix/pools/" + poolName, Config: []byte(`{}`),
	}); err != nil {
		t.Fatalf("CreateStoragePool: %v", err)
	}

	// A scheduled VM pinned to the source node, with a disk row so the GET
	// projection resolves (a scheduled VM without a disk is a 500).
	vmID := uuid.New()
	vmName := "mig-vm-" + uuid.NewString()[:8]
	spec, err := store.MarshalSchedulingSpec(store.SchedulingSpec{PoolName: poolName})
	if err != nil {
		t.Fatalf("MarshalSchedulingSpec: %v", err)
	}
	srcID := source.ID
	vm := store.VM{
		ID: vmID, OwnerID: opID, Name: vmName,
		DesiredPhase: store.VmDesiredPhaseRunning, Architecture: store.CpuArchAmd64,
		SchedulingStatus: store.VMSchedulingScheduled, SchedulingSpec: spec,
		PinnedNodeID: &srcID,
		CpuCores:     2, MemoryMib: 2048,
		Labels: []byte(`{}`), ImageFormat: store.ImageFormatQcow2,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	seedVMRow(t, vm)
	seedVMDisk(t, vmID, poolID)

	// The TARGET agent has already overwritten current_node_id to itself
	// (mid-migration heartbeat, before cutover).
	rt := store.VMRuntime{VmID: vmID, CurrentNodeID: &target.ID, Phase: store.VmPhaseRunning}
	if err := sharedEtcdClient.PutJSON(ctx, etcd.Key("vm_runtime", vmID.String()), rt); err != nil {
		t.Fatalf("seed vm_runtime: %v", err)
	}

	// A non-terminal migration: source A -> target B.
	seedActiveMigration(t, vmID, source.ID, target.ID)

	resp := h.get(t, "/v1/vms/"+vmName, opToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("vm get status = %d, want 200", resp.StatusCode)
	}
	var view struct {
		Node   *string `json:"node"`
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	decodeJSON(t, resp, &view)

	// (a) Displayed node is the SOURCE, not the heartbeat-overwritten target.
	if view.Node == nil {
		t.Fatalf("node = nil, want %q (source)", source.Name)
	}
	if *view.Node != source.Name {
		t.Errorf("node = %q, want %q (source, not target %q)", *view.Node, source.Name, target.Name)
	}
	// M1 still holds: an in-flight migration projects status.phase=migrating.
	if view.Status.Phase != "migrating" {
		t.Errorf("status.phase = %q, want migrating", view.Status.Phase)
	}
}

// createReadyNode creates a ready node with a recognizable name prefix.
func createReadyNode(t *testing.T, s *etcdstore.Store, role string) store.Node {
	t.Helper()
	n, err := s.CreateNode(context.Background(), store.CreateNodeParams{
		ID:                      uuid.New(),
		Name:                    role + "-node-" + uuid.NewString()[:8],
		Architecture:            store.CpuArchAmd64,
		AdvertisedEndpoint:      "https://node:9443",
		MigrationHost:           "10.0.0.5",
		MigrationPortRangeStart: 49152,
		MigrationPortRangeEnd:   49251,
		Status:                  store.NodeStatusReady,
	})
	if err != nil {
		t.Fatalf("CreateNode(%s): %v", role, err)
	}
	return n
}

// seedVMRow writes the vms primary row + name guard, mirroring seedScheduledVM.
func seedVMRow(t *testing.T, vm store.VM) {
	t.Helper()
	ctx := context.Background()
	if err := sharedEtcdClient.PutJSON(ctx, etcd.Key("vms", vm.ID.String()), vm); err != nil {
		t.Fatalf("seed vm: %v", err)
	}
	if err := sharedEtcdClient.Put(ctx, etcd.Key("uniq", "vms", "name", vm.Name), []byte(vm.ID.String())); err != nil {
		t.Fatalf("seed vm name guard: %v", err)
	}
}

// seedVMDisk writes a single vm_disks primary row + its per-VM index leaf so the
// GET projection resolves the disk (and thus the pool name).
func seedVMDisk(t *testing.T, vmID, poolID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	diskID := uuid.New()
	now := time.Now().UTC()
	disk := store.VMDisk{
		ID: diskID, VmID: vmID, StoragePoolID: poolID, DeviceOrder: 0,
		Bus: store.DiskBusVirtio, SizeGib: 10, SourceKind: "image",
		Format: store.ImageFormatQcow2, CacheMode: store.DiskCacheModeNone,
		Discard: store.DiskDiscardUnmap, CreatedAt: now, UpdatedAt: now,
	}
	if err := sharedEtcdClient.PutJSON(ctx, etcd.Key("vm_disks", diskID.String()), disk); err != nil {
		t.Fatalf("seed vm_disk: %v", err)
	}
	if err := sharedEtcdClient.Put(ctx, etcd.Key("index", "vm_disks", "vm", vmID.String(), diskID.String()), []byte(diskID.String())); err != nil {
		t.Fatalf("seed vm_disk index: %v", err)
	}
}

// seedActiveMigration writes a non-terminal migration primary row + its per-VM
// index leaf, so ActiveMigrationForVM returns it. This is the minimal shape the
// VM status overlay reads; it bypasses CreateMigration (which also enqueues a
// task/job not needed here).
func seedActiveMigration(t *testing.T, vmID, sourceNodeID, targetNodeID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	migID := uuid.New()
	now := time.Now().UTC()
	m := store.Migration{
		ID: migID, VmID: vmID,
		SourceNodeID: &sourceNodeID, TargetNodeID: &targetNodeID,
		Phase:     store.MigrationPhaseActive,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := sharedEtcdClient.PutJSON(ctx, etcd.Key("migrations", migID.String()), m); err != nil {
		t.Fatalf("seed migration: %v", err)
	}
	if err := sharedEtcdClient.Put(ctx, etcd.Key("index", "migrations", "vm", vmID.String(), migID.String()), []byte(migID.String())); err != nil {
		t.Fatalf("seed migration vm index: %v", err)
	}
}
