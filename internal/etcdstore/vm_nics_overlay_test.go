// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// seedOverlayNetwork creates a type=overlay network (which forces VNI
// allocation in CreateNetwork) and returns the persisted row, including its
// CP-assigned VNI.
func seedOverlayNetwork(t *testing.T, s *etcdstore.Store) store.Network {
	t.Helper()
	n, err := s.CreateNetwork(context.Background(), store.CreateNetworkParams{
		ID:     uuid.New(),
		Name:   uniqueNetName("ov"),
		Type:   store.NetworkTypeOverlay,
		Config: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("seedOverlayNetwork: %v", err)
	}
	if n.VNI == nil {
		t.Fatalf("seedOverlayNetwork: overlay network has nil VNI")
	}
	return n
}

// seedBridgeNetwork creates a type=bridge network. Its NICs must never surface
// in the overlay placement projection.
func seedBridgeNetwork(t *testing.T, s *etcdstore.Store) store.Network {
	t.Helper()
	n, err := s.CreateNetwork(context.Background(), store.CreateNetworkParams{
		ID:         uuid.New(),
		Name:       uniqueNetName("br"),
		Type:       store.NetworkTypeBridge,
		BridgeName: "br0",
		Mtu:        1500,
		Config:     []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("seedBridgeNetwork: %v", err)
	}
	return n
}

// seedVMWithNIC writes a vm_nic row and its per-network delete-block index entry
// directly through the raw client - exactly the two keys the placement
// projection reads (the NIC row plus the network index that enumerates it). This
// avoids the full CreateScheduledVM path, which gates the NIC behind a scheduled
// placement fixture the projection does not consult. It returns the
// owning VM id so the caller can place (or not place) the VM via placeVM.
func seedVMWithNIC(t *testing.T, cli *etcd.Client, networkID uuid.UUID, mac string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	vmID := uuid.New()
	nicID := uuid.New()
	nic := store.VMNic{
		ID:         nicID,
		VmID:       vmID,
		NetworkID:  networkID,
		Model:      store.NicModelVirtio,
		MacAddress: mustMAC(t, mac),
		Generation: 1,
	}
	if err := cli.PutJSON(ctx, etcd.Key("vm_nics", nicID.String()), nic); err != nil {
		t.Fatalf("seedVMWithNIC: put nic: %v", err)
	}
	idxKey := etcd.Key("index", "vm_nics", "network", networkID.String(), nicID.String())
	if err := cli.Put(ctx, idxKey, []byte(nicID.String())); err != nil {
		t.Fatalf("seedVMWithNIC: put network index: %v", err)
	}
	return vmID
}

// placeVM writes the observed runtime row pinning the VM to a node, the single
// runtime field the placement projection joins on.
func placeVM(t *testing.T, cli *etcd.Client, vmID, nodeID uuid.UUID) {
	t.Helper()
	rt := store.VMRuntime{VmID: vmID, CurrentNodeID: &nodeID, Phase: store.VmPhaseRunning}
	if err := cli.PutJSON(context.Background(), etcd.Key("vm_runtime", vmID.String()), rt); err != nil {
		t.Fatalf("placeVM: %v", err)
	}
}

// TestListOverlayNICPlacements verifies the projection returns one entry per NIC
// on a type=overlay network whose owning VM has a current node, carrying the
// network's VNI, the NIC's MAC, and the owning node. Bridge NICs and unplaced
// overlay NICs are excluded.
func TestListOverlayNICPlacements(t *testing.T) {
	ctx := context.Background()
	s, cli := startStore(t)

	nodeA, nodeB := uuid.New(), uuid.New()
	ov := seedOverlayNetwork(t, s)
	br := seedBridgeNetwork(t, s)
	vmA := seedVMWithNIC(t, cli, ov.ID, "52:54:00:00:00:0a")
	vmB := seedVMWithNIC(t, cli, ov.ID, "52:54:00:00:00:0b")
	vmC := seedVMWithNIC(t, cli, br.ID, "52:54:00:00:00:0c") // bridge NIC - excluded
	vmD := seedVMWithNIC(t, cli, ov.ID, "52:54:00:00:00:0d") // overlay NIC, no runtime - excluded
	placeVM(t, cli, vmA, nodeA)
	placeVM(t, cli, vmB, nodeB)
	placeVM(t, cli, vmC, nodeA)
	_ = vmD // intentionally not placed -> excluded

	got, err := s.ListOverlayNICPlacements(ctx)
	if err != nil {
		t.Fatalf("ListOverlayNICPlacements: %v", err)
	}
	want := map[string]uuid.UUID{ // mac -> node
		"52:54:00:00:00:0a": nodeA,
		"52:54:00:00:00:0b": nodeB,
	}
	if len(got) != len(want) {
		t.Fatalf("placements = %d, want %d: %+v", len(got), len(want), got)
	}
	for _, p := range got {
		if p.VNI != *ov.VNI {
			t.Errorf("VNI = %d, want %d", p.VNI, *ov.VNI)
		}
		wantNode, ok := want[p.Mac.String()]
		if !ok {
			t.Errorf("unexpected mac %s in placements", p.Mac)
			continue
		}
		if p.NodeID != wantNode {
			t.Errorf("mac %s node = %s, want %s", p.Mac, p.NodeID, wantNode)
		}
	}
}
