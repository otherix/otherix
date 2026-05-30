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

func networkParams(id uuid.UUID, name string) store.CreateNetworkParams {
	return store.CreateNetworkParams{
		ID:         id,
		Name:       name,
		Type:       store.NetworkType("bridge"),
		BridgeName: "br-test",
		VlanTag:    nil,
		Mtu:        1500,
		Config:     []byte("{}"),
	}
}

func uniqueNetworkName(prefix string) string {
	return prefix + "-" + uuid.NewString()[:8]
}

func TestNetworkByIDNotFound(t *testing.T) {
	requireSharedHarness(t)
	s := newStore(t, sharedHarness)
	if _, err := s.NetworkByID(context.Background(), uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("NetworkByID(absent) error = %v, want store.ErrNotFound", err)
	}
}

func TestCreateNetworkDuplicateName(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	name := uniqueNetworkName("dup")
	if _, err := s.CreateNetwork(ctx, networkParams(uuid.New(), name)); err != nil {
		t.Fatalf("first CreateNetwork: %v", err)
	}
	_, err := s.CreateNetwork(ctx, networkParams(uuid.New(), name))
	if !errors.Is(err, store.ErrNetworkNameExists) {
		t.Errorf("duplicate CreateNetwork error = %v, want store.ErrNetworkNameExists", err)
	}
}

func TestDeleteNetworkNotFound(t *testing.T) {
	requireSharedHarness(t)
	s := newStore(t, sharedHarness)
	if err := s.DeleteNetwork(context.Background(), uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DeleteNetwork(absent) error = %v, want store.ErrNotFound", err)
	}
}

func TestDeleteNetworkSucceeds(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	id := uuid.New()
	if _, err := s.CreateNetwork(ctx, networkParams(id, uniqueNetworkName("del"))); err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	if err := s.DeleteNetwork(ctx, id); err != nil {
		t.Fatalf("DeleteNetwork: %v", err)
	}
	if _, err := s.NetworkByID(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("after delete NetworkByID error = %v, want store.ErrNotFound", err)
	}
}

func TestDeleteNetworkInUse(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	netID := uuid.New()
	if _, err := s.CreateNetwork(ctx, networkParams(netID, uniqueNetworkName("inuse"))); err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}

	ownerID := uuid.New()
	if _, err := s.Pool().Exec(ctx,
		`insert into users (id, email, password_hash, role) values ($1, $2, 'x', 'developer')`,
		ownerID, "netinuse-"+uuid.NewString()+"@example.test"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	vmID := uuid.New()
	if _, err := s.Pool().Exec(ctx,
		`insert into vms (id, owner_id, name, architecture, cpu_cores, memory_mib, machine_type)
		 values ($1, $2, $3, 'amd64', 1, 256, 'pc-i440fx-8.0')`,
		vmID, ownerID, "vm-"+uuid.NewString()[:8]); err != nil {
		t.Fatalf("insert vm: %v", err)
	}
	if _, err := s.Pool().Exec(ctx,
		`insert into vm_nics (id, vm_id, network_id, device_order, mac_address)
		 values ($1, $2, $3, 0, '52:54:00:12:34:56'::macaddr)`,
		uuid.New(), vmID, netID); err != nil {
		t.Fatalf("insert vm_nic: %v", err)
	}

	err := s.DeleteNetwork(ctx, netID)
	var inUse *store.ResourceInUseError
	if !errors.As(err, &inUse) {
		t.Fatalf("DeleteNetwork(in use) error = %v, want *store.ResourceInUseError", err)
	}
	if inUse.Resources["vm_nics"] != 1 {
		t.Errorf("Resources[vm_nics] = %d, want 1", inUse.Resources["vm_nics"])
	}
}
