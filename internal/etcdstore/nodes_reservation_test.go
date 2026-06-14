// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// seedReadyNodeWithCapacity writes a ready node row directly with reported
// availability and a recent heartbeat, so VMs are not counted as pending against
// it. Returns the node id.
func seedReadyNodeWithCapacity(t *testing.T, cli *etcd.Client, name string, cpu int32, mem int64) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	hb := time.Now().UTC()
	cpuAvail := cpu
	memAvail := mem
	id := uuid.New()
	n := store.Node{
		ID:                 id,
		Name:               name,
		Architecture:       store.CpuArchAmd64,
		AdvertisedEndpoint: "https://res.example:9443",
		MigrationHost:      "10.0.0.9",
		Status:             store.NodeStatusReady,
		CPUCoresAvailable:  &cpuAvail,
		MemoryAvailableMib: &memAvail,
		LastHeartbeatAt:    &hb,
		CreatedAt:          time.Now().UTC(),
		UpdatedAt:          time.Now().UTC(),
	}
	if err := cli.PutJSON(ctx, etcd.Key("nodes", id.String()), n); err != nil {
		t.Fatalf("seed node %q: %v", name, err)
	}
	return id
}

// TestNodeEffective_ReservesActiveMigrationTarget proves the target node's
// effective availability is reduced by an in-flight migration's VM size, even
// though that VM is pinned to the SOURCE (not the target) and so does not appear
// in the target's pending-reservation set.
func TestNodeEffective_ReservesActiveMigrationTarget(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()

	nodeA := seedReadyNodeWithCapacity(t, cli, uniqueNodeName("res-src"), 16, 32768)
	nodeB := seedReadyNodeWithCapacity(t, cli, uniqueNodeName("res-tgt"), 16, 32768)

	// VM of size S=4cpu/8192mib pinned to nodeA (the source), migrating to nodeB.
	vm := seedPinnedVM(t, cli, nodeA)
	if vm.CpuCores != 2 || vm.MemoryMib != 2048 {
		t.Fatalf("vmRow size = %d/%d, want 2/2048", vm.CpuCores, vm.MemoryMib)
	}
	// Bump the migrating VM to a known size so the subtraction is unambiguous.
	vm.CpuCores = 4
	vm.MemoryMib = 8192
	if err := cli.PutJSON(ctx, etcd.Key("vms", vm.ID.String()), vm); err != nil {
		t.Fatalf("resize vm: %v", err)
	}

	_ = seedActiveMigration(t, s, vm.ID, nodeA, nodeB)

	// The migrating VM is pinned to nodeA, so nodeA's effective view already
	// counts it via the normal pinned/pending path - assert nodeB carries the
	// migration-target reservation instead.
	effB, err := s.NodeEffectiveByID(ctx, nodeB)
	if err != nil {
		t.Fatalf("NodeEffectiveByID(nodeB): %v", err)
	}
	if effB.CPUCoresEffective == nil || *effB.CPUCoresEffective != 12 {
		t.Errorf("nodeB CPUCoresEffective = %v, want 12 (16-4 reserved for migration)", effB.CPUCoresEffective)
	}
	if effB.MemoryEffectiveMib == nil || *effB.MemoryEffectiveMib != 24576 {
		t.Errorf("nodeB MemoryEffectiveMib = %v, want 24576 (32768-8192 reserved for migration)", effB.MemoryEffectiveMib)
	}
}

// TestNodeEffective_NoMigrationsUnchanged proves the migration-target
// subtraction is a no-op when no migration targets the node: effective equals
// reported availability, identical to pre-Task-7 behaviour.
func TestNodeEffective_NoMigrationsUnchanged(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()

	nodeB := seedReadyNodeWithCapacity(t, cli, uniqueNodeName("res-noop"), 16, 32768)

	effB, err := s.NodeEffectiveByID(ctx, nodeB)
	if err != nil {
		t.Fatalf("NodeEffectiveByID(nodeB): %v", err)
	}
	if effB.CPUCoresEffective == nil || *effB.CPUCoresEffective != 16 {
		t.Errorf("nodeB CPUCoresEffective = %v, want 16 (full, no migrations)", effB.CPUCoresEffective)
	}
	if effB.MemoryEffectiveMib == nil || *effB.MemoryEffectiveMib != 32768 {
		t.Errorf("nodeB MemoryEffectiveMib = %v, want 32768 (full, no migrations)", effB.MemoryEffectiveMib)
	}
}
