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

func nodeParams(id uuid.UUID, name string) store.CreateNodeParams {
	return store.CreateNodeParams{
		ID:                      id,
		Name:                    name,
		Architecture:            store.CpuArchAmd64,
		AdvertisedEndpoint:      "https://" + name + ".local:8443",
		MigrationHost:           name + ".local",
		MigrationPortRangeStart: 49152,
		MigrationPortRangeEnd:   49251,
		Status:                  store.NodeStatusPending,
	}
}

func TestNodeEffectiveByIDNotFound(t *testing.T) {
	requireSharedHarness(t)
	s := newStore(t, sharedHarness)
	if _, err := s.NodeEffectiveByID(context.Background(), uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("NodeEffectiveByID(absent) error = %v, want store.ErrNotFound", err)
	}
}

func TestCreateNodeDuplicateName(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	name := uniqueNodeName()
	if _, err := s.CreateNode(ctx, nodeParams(uuid.New(), name)); err != nil {
		t.Fatalf("first CreateNode: %v", err)
	}
	_, err := s.CreateNode(ctx, nodeParams(uuid.New(), name))
	if !errors.Is(err, store.ErrNodeNameExists) {
		t.Errorf("duplicate CreateNode error = %v, want store.ErrNodeNameExists", err)
	}
}

func TestDeleteNodeSucceeds(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	id := uuid.New()
	if _, err := s.CreateNode(ctx, nodeParams(id, uniqueNodeName())); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	out, err := s.DeleteNode(ctx, id, false, uuid.New())
	if err != nil {
		t.Fatalf("DeleteNode: %v", err)
	}
	if out.VMsOrphaned != 0 || out.MigrationsCancelled != 0 {
		t.Errorf("non-force DeleteNode outcome = %+v, want zeros", out)
	}
	if _, err := s.NodeEffectiveByID(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("after delete NodeEffectiveByID error = %v, want store.ErrNotFound", err)
	}
}

func TestDeleteNodeBlockedByVMRuntime(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	nodeID := uuid.New()
	if _, err := s.CreateNode(ctx, nodeParams(nodeID, uniqueNodeName())); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	ownerID := uuid.New()
	if _, err := s.Pool().Exec(ctx,
		`insert into users (id, email, password_hash, role) values ($1, $2, 'x', 'developer')`,
		ownerID, "nodeinuse-"+uuid.NewString()+"@example.test"); err != nil {
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
		`insert into vm_runtime (vm_id, current_node_id, phase) values ($1, $2, 'running')`,
		vmID, nodeID); err != nil {
		t.Fatalf("insert vm_runtime: %v", err)
	}

	_, err := s.DeleteNode(ctx, nodeID, false, uuid.New())
	var inUse *store.ResourceInUseError
	if !errors.As(err, &inUse) {
		t.Fatalf("DeleteNode(blocked) error = %v, want *store.ResourceInUseError", err)
	}
	if inUse.Resources["vms"] != 1 {
		t.Errorf("Resources[vms] = %d, want 1", inUse.Resources["vms"])
	}
	if _, ok := inUse.Resources["active_migrations"]; !ok {
		t.Error("Resources missing active_migrations key (nodes envelope lists both)")
	}
}
