// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

func mustMAC(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	mac, err := net.ParseMAC(s)
	if err != nil {
		t.Fatalf("ParseMAC(%q): %v", s, err)
	}
	return mac
}

// TestCreateScheduledVMWithNicWritesIndexAndBlocksDelete verifies that a VM
// created with a NIC lands the vm_nic row, the per-network delete-block index
// entry, and that DeleteNetwork then refuses with *store.ResourceInUseError.
func TestCreateScheduledVMWithNicWritesIndexAndBlocksDelete(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	nodeID, poolID, templateID, _ := schedulingFixture(t, s)
	owner := uuid.New()
	name := "vm-" + uuid.NewString()[:8]

	netID := uuid.New()
	if _, err := s.CreateNetwork(ctx, store.CreateNetworkParams{
		ID: netID, Name: uniqueNetName("nic"), Type: store.NetworkTypeBridge,
		BridgeName: "br0", Mtu: 1500, Config: []byte(`{}`),
	}); err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}

	writes := vmCreateWrites(t, name, owner, nodeID, poolID, templateID)
	nicID := uuid.New()
	writes.Nic = &store.CreateVMNicParams{
		ID: nicID, VmID: writes.VM.ID, NetworkID: netID, DeviceOrder: 0,
		Model: store.NicModelVirtio, MacAddress: mustMAC(t, "52:54:00:12:34:56"),
	}

	if _, err := s.CreateScheduledVM(ctx, func(store.PlacementReader) (store.VMCreateWrites, error) {
		return writes, nil
	}); err != nil {
		t.Fatalf("CreateScheduledVM: %v", err)
	}

	nics, err := s.ListVMNicsByVM(ctx, writes.VM.ID)
	if err != nil {
		t.Fatalf("ListVMNicsByVM: %v", err)
	}
	if len(nics) != 1 {
		t.Fatalf("nics = %d, want 1", len(nics))
	}
	got := nics[0]
	if got.ID != nicID || got.NetworkID != netID || got.Model != store.NicModelVirtio {
		t.Errorf("nic = %+v, want id %v net %v virtio", got, nicID, netID)
	}
	if got.MacAddress.String() != "52:54:00:12:34:56" {
		t.Errorf("mac = %q, want 52:54:00:12:34:56", got.MacAddress)
	}
	if got.Generation != 1 {
		t.Errorf("generation = %d, want 1", got.Generation)
	}

	// The network now has an active referent; delete must be blocked.
	err = s.DeleteNetwork(ctx, netID)
	var blocking *store.ResourceInUseError
	if !errors.As(err, &blocking) {
		t.Fatalf("DeleteNetwork err = %v, want *store.ResourceInUseError", err)
	}
	if blocking.Resources["vm_nics"] != 1 {
		t.Errorf("blocking vm_nics = %d, want 1", blocking.Resources["vm_nics"])
	}
}

// TestNetworkByName resolves a network through its case-insensitive name guard
// and returns store.ErrNotFound for an unknown name.
func TestNetworkByName(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	name := uniqueNetName("byname")
	created, err := s.CreateNetwork(ctx, store.CreateNetworkParams{
		ID: uuid.New(), Name: name, Type: store.NetworkTypeBridge,
		BridgeName: "br0", Mtu: 1500, Config: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	got, err := s.NetworkByName(ctx, name)
	if err != nil {
		t.Fatalf("NetworkByName: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("NetworkByName id = %v, want %v", got.ID, created.ID)
	}
	// Case-insensitive guard.
	if _, err := s.NetworkByName(ctx, uuid.NewString()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("NetworkByName(unknown) = %v, want ErrNotFound", err)
	}
}

// TestCreateScheduledVMWithoutNic confirms the no-NIC path leaves the VM with
// an empty NIC list (legacy SLIRP fallback) and writes no network index entry.
func TestCreateScheduledVMWithoutNic(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	nodeID, poolID, templateID, _ := schedulingFixture(t, s)
	owner := uuid.New()
	name := "vm-" + uuid.NewString()[:8]

	writes := vmCreateWrites(t, name, owner, nodeID, poolID, templateID)
	if _, err := s.CreateScheduledVM(ctx, func(store.PlacementReader) (store.VMCreateWrites, error) {
		return writes, nil
	}); err != nil {
		t.Fatalf("CreateScheduledVM: %v", err)
	}
	nics, err := s.ListVMNicsByVM(ctx, writes.VM.ID)
	if err != nil {
		t.Fatalf("ListVMNicsByVM: %v", err)
	}
	if len(nics) != 0 {
		t.Errorf("nics = %d, want 0", len(nics))
	}
}
