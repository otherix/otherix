// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"errors"
	"net/netip"
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
	err := s.RunHeartbeatProjection(ctx, func(hp store.HeartbeatProjection) error {
		pre, err := hp.NodeForHeartbeat(ctx, node.ID)
		if err != nil {
			return err
		}
		if pre.ID != node.ID {
			t.Errorf("NodeForHeartbeat id = %v, want %v", pre.ID, node.ID)
		}
		if err := hp.UpdateNodeHeartbeat(ctx, store.UpdateNodeHeartbeatParams{
			ID: node.ID, MigrationHost: "10.1.1.1", MigrationPortRangeStart: 49152, MigrationPortRangeEnd: 49251,
			CPUCoresTotal: &cores, CPUCoresAvailable: &cores, MemoryTotalMib: &mem, MemoryAvailableMib: &mem,
		}); err != nil {
			return err
		}
		mc := int32(1)
		if err := hp.UpdateNodeMemoryPressure(ctx, store.UpdateNodeMemoryPressureParams{ID: node.ID, MemoryPressureSince: since, MemoryPressureCount: mc}); err != nil {
			return err
		}
		// Firmware catalogue lookup + node-firmware upsert.
		fwID, err := hp.LookupFirmwareByCatalog(ctx, store.LookupFirmwareByCatalogParams{Name: fw.Name, Architecture: store.CpuArchAmd64, Type: store.FirmwareTypeUefi})
		if err != nil {
			return err
		}
		if fwID != fw.ID {
			t.Errorf("LookupFirmwareByCatalog = %v, want %v", fwID, fw.ID)
		}
		if err := hp.UpsertNodeFirmware(ctx, store.UpsertNodeFirmwareParams{NodeID: node.ID, FirmwareID: fwID, CodePath: "/fw/code", Available: true}); err != nil {
			return err
		}
		// Filter existing VM ids.
		existing, err := hp.FilterExistingVMIDs(ctx, []uuid.UUID{vm.ID, uuid.New()})
		if err != nil {
			return err
		}
		if len(existing) != 1 || existing[0] != vm.ID {
			t.Errorf("FilterExistingVMIDs = %v, want [%v]", existing, vm.ID)
		}
		// Runtime upsert binds the VM to the node.
		if err := hp.UpsertVMRuntime(ctx, store.UpsertVMRuntimeParams{VmID: vm.ID, CurrentNodeID: &node.ID, Phase: store.VmPhaseRunning, ObservedGeneration: 1}); err != nil {
			return err
		}
		// Pool reconciliation report.
		if err := hp.UpdateStoragePoolReconciliation(ctx, store.UpdateStoragePoolReconciliationParams{NodeID: node.ID, Name: pool.Name, ReconciliationStatus: "ok"}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RunHeartbeatProjection: %v", err)
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
	if err := s.RunHeartbeatProjection(ctx, func(hp store.HeartbeatProjection) error {
		var e error
		declared, e = hp.ListVMsForNodeDeclared(ctx, node.ID)
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
	if err := s.RunHeartbeatProjection(ctx, func(hp store.HeartbeatProjection) error {
		_, e := hp.NodeForHeartbeat(ctx, uuid.New())
		return e
	}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("NodeForHeartbeat(absent) = %v, want store.ErrNotFound", err)
	}
}

// TestHeartbeatDeclaredNetworksClusterWide verifies the projection hands the
// FULL set of non-deleted networks to every node (networks are cluster-wide,
// not node-scoped). Two distinct nodes both observe the same network set,
// soft-deleted networks are excluded.
func TestHeartbeatDeclaredNetworksClusterWide(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	nodeA := nodeParams(uniqueNodeName("hbnetA"))
	if _, err := s.CreateNode(ctx, nodeA); err != nil {
		t.Fatalf("CreateNode A: %v", err)
	}
	nodeB := nodeParams(uniqueNodeName("hbnetB"))
	if _, err := s.CreateNode(ctx, nodeB); err != nil {
		t.Fatalf("CreateNode B: %v", err)
	}

	// A nat network with subnet/gateway and a plain managed bridge.
	prefix := netip.MustParsePrefix("10.20.0.0/24")
	gw := netip.MustParseAddr("10.20.0.1")
	natNet := netParams(uniqueNetName("hbnat"))
	natNet.Managed = true
	natNet.Egress = store.NetworkEgressNAT
	natNet.Subnet = &prefix
	natNet.Gateway = &gw
	if _, err := s.CreateNetwork(ctx, natNet); err != nil {
		t.Fatalf("CreateNetwork nat: %v", err)
	}
	plainNet := netParams(uniqueNetName("hbplain"))
	plainNet.Managed = true
	if _, err := s.CreateNetwork(ctx, plainNet); err != nil {
		t.Fatalf("CreateNetwork plain: %v", err)
	}
	// A soft-deleted network must NOT surface.
	goneNet := netParams(uniqueNetName("hbgone"))
	if _, err := s.CreateNetwork(ctx, goneNet); err != nil {
		t.Fatalf("CreateNetwork gone: %v", err)
	}
	if err := s.DeleteNetwork(ctx, goneNet.ID); err != nil {
		t.Fatalf("DeleteNetwork gone: %v", err)
	}

	listVia := func(nodeID uuid.UUID) map[uuid.UUID]store.Network {
		t.Helper()
		var got []store.Network
		if err := s.RunHeartbeatProjection(ctx, func(hp store.HeartbeatProjection) error {
			// NodeForHeartbeat exercises the per-node path; ListNetworks takes
			// NO node argument, proving it is not node-filtered.
			if _, e := hp.NodeForHeartbeat(ctx, nodeID); e != nil {
				return e
			}
			var e error
			got, e = hp.ListNetworks(ctx)
			return e
		}); err != nil {
			t.Fatalf("ListNetworks via node %v: %v", nodeID, err)
		}
		out := make(map[uuid.UUID]store.Network, len(got))
		for _, n := range got {
			out[n.ID] = n
		}
		return out
	}

	a := listVia(nodeA.ID)
	b := listVia(nodeB.ID)

	for _, want := range []uuid.UUID{natNet.ID, plainNet.ID} {
		if _, ok := a[want]; !ok {
			t.Errorf("node A declared networks missing %v", want)
		}
		if _, ok := b[want]; !ok {
			t.Errorf("node B declared networks missing %v", want)
		}
	}
	if _, ok := a[goneNet.ID]; ok {
		t.Errorf("soft-deleted network %v leaked into declared set", goneNet.ID)
	}
	// Cluster-wide: both nodes see the identical set of network ids.
	if len(a) != len(b) {
		t.Errorf("node A saw %d networks, node B saw %d; want equal cluster-wide sets", len(a), len(b))
	}
	// nat network carries subnet + gateway through the projection.
	if n, ok := a[natNet.ID]; ok {
		if n.Subnet == nil || n.Subnet.String() != "10.20.0.0/24" {
			t.Errorf("nat network subnet = %v, want 10.20.0.0/24", n.Subnet)
		}
		if n.Gateway == nil || n.Gateway.String() != "10.20.0.1" {
			t.Errorf("nat network gateway = %v, want 10.20.0.1", n.Gateway)
		}
		if n.Egress != store.NetworkEgressNAT {
			t.Errorf("nat network egress = %v, want nat", n.Egress)
		}
	}
}
