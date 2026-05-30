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

	heartbeathandlers "github.com/otherix/otherix/internal/api/handlers/heartbeat"
	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// The etcd store satisfies the heartbeat handler contract.
var _ heartbeathandlers.Store = (*etcdstore.Store)(nil)

func TestInHeartbeatProjection(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()

	node := nodeParams(uniqueNodeName("hb"))
	if _, err := s.CreateNode(ctx, node); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	fw := fwParams(uniqueFwName("hbfw"), store.CpuArchAmd64, false)
	if _, err := s.CreateFirmware(ctx, fw); err != nil {
		t.Fatalf("CreateFirmware: %v", err)
	}
	pool := poolParams(node.ID, uniquePoolName("hbpool"))
	if _, err := s.CreateStoragePool(ctx, pool); err != nil {
		t.Fatalf("CreateStoragePool: %v", err)
	}
	// A live VM for the runtime upsert.
	vm := vmRow(uniqueNodeName("hbvm"))
	seedVM(t, cli, vm)

	cores := int32(8)
	mem := int64(16384)
	since := store.Node{}.MemoryPressureSince // nil
	err := s.InHeartbeatTx(ctx, func(tx store.HeartbeatTx) error {
		pre, err := tx.NodeForHeartbeat(ctx, node.ID)
		if err != nil {
			return err
		}
		if pre.ID != node.ID {
			t.Errorf("NodeForHeartbeat id = %v, want %v", pre.ID, node.ID)
		}
		if err := tx.UpdateNodeHeartbeat(ctx, store.UpdateNodeHeartbeatParams{
			ID: node.ID, MigrationHost: "10.1.1.1", MigrationPortRangeStart: 49152, MigrationPortRangeEnd: 49251,
			CPUCoresTotal: &cores, CPUCoresAvailable: &cores, MemoryTotalMib: &mem, MemoryAvailableMib: &mem,
		}); err != nil {
			return err
		}
		mc := int32(1)
		if err := tx.UpdateNodeMemoryPressure(ctx, store.UpdateNodeMemoryPressureParams{ID: node.ID, MemoryPressureSince: since, MemoryPressureCount: mc}); err != nil {
			return err
		}
		// Firmware catalogue lookup + node-firmware upsert.
		fwID, err := tx.LookupFirmwareByCatalog(ctx, store.LookupFirmwareByCatalogParams{Name: fw.Name, Architecture: store.CpuArchAmd64, Type: store.FirmwareTypeUefi})
		if err != nil {
			return err
		}
		if fwID != fw.ID {
			t.Errorf("LookupFirmwareByCatalog = %v, want %v", fwID, fw.ID)
		}
		if err := tx.UpsertNodeFirmware(ctx, store.UpsertNodeFirmwareParams{NodeID: node.ID, FirmwareID: fwID, CodePath: "/fw/code", Available: true}); err != nil {
			return err
		}
		// Filter existing VM ids.
		existing, err := tx.FilterExistingVMIDs(ctx, []uuid.UUID{vm.ID, uuid.New()})
		if err != nil {
			return err
		}
		if len(existing) != 1 || existing[0] != vm.ID {
			t.Errorf("FilterExistingVMIDs = %v, want [%v]", existing, vm.ID)
		}
		// Runtime upsert binds the VM to the node.
		if err := tx.UpsertVMRuntime(ctx, store.UpsertVMRuntimeParams{VmID: vm.ID, CurrentNodeID: &node.ID, Phase: store.VmPhaseRunning, ObservedGeneration: 1}); err != nil {
			return err
		}
		// Pool reconciliation report.
		if err := tx.UpdateStoragePoolReconciliation(ctx, store.UpdateStoragePoolReconciliationParams{NodeID: node.ID, Name: pool.Name, ReconciliationStatus: "ok"}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("InHeartbeatTx: %v", err)
	}

	// Capability fields landed.
	n, err := s.NodeByID(ctx, node.ID)
	if err != nil {
		t.Fatalf("NodeByID: %v", err)
	}
	if n.CPUCoresTotal == nil || *n.CPUCoresTotal != 8 || n.LastHeartbeatAt == nil || n.MemoryPressureCount != 1 {
		t.Errorf("node after heartbeat = %+v", n)
	}
	// node_firmware projected -> ListNodeFirmwares returns it.
	nfs, err := s.ListNodeFirmwares(ctx, store.ListNodeFirmwaresParams{NodeID: node.ID, LimitCount: 200})
	if err != nil || len(nfs) != 1 {
		t.Errorf("ListNodeFirmwares = (%v, %v), want 1", nfs, err)
	}
	// Runtime + node index landed -> ListVMsForNodeDeclared sees the VM, and the
	// effective-availability/node-delete index is populated.
	var declared []store.ListVMsForNodeDeclaredRow
	if err := s.InHeartbeatTx(ctx, func(tx store.HeartbeatTx) error {
		var e error
		declared, e = tx.ListVMsForNodeDeclared(ctx, node.ID)
		return e
	}); err != nil {
		t.Fatalf("ListVMsForNodeDeclared: %v", err)
	}
	if len(declared) != 1 || declared[0].Name != vm.Name {
		t.Errorf("declared vms = %+v, want [%s]", declared, vm.Name)
	}
	// The runtime node index DeleteNode consumes is populated.
	idxVal, found, err := cli.Get(ctx, etcd.Key("index", "vm_runtime", "node", node.ID.String(), vm.ID.String()))
	if err != nil || !found || string(idxVal) != vm.ID.String() {
		t.Errorf("vm_runtime node index = (%q, %v, %v), want vm id present", idxVal, found, err)
	}
	// Pool reconciliation status landed.
	pools, err := s.StoragePoolsByName(ctx, pool.Name)
	if err != nil || len(pools) != 1 || pools[0].ReconciliationStatus != "ok" {
		t.Errorf("pool reconciliation = (%+v, %v), want status ok", pools, err)
	}

	// NodeForHeartbeat on an absent node is not-found.
	if err := s.InHeartbeatTx(ctx, func(tx store.HeartbeatTx) error {
		_, e := tx.NodeForHeartbeat(ctx, uuid.New())
		return e
	}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("NodeForHeartbeat(absent) = %v, want store.ErrNotFound", err)
	}
}
