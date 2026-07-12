// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration

package etcdstore_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/handlers/vms"
	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/scheduler"
	"github.com/otherix/otherix/internal/store"
)

// seedReadyNodeWithPool seeds a ready node with reported total + available
// capacity and a heartbeat in the PAST, then a "default" local_dir pool on it.
// The past heartbeat is load-bearing: pendingReservations counts a pinned VM
// against a node only when the VM's CreatedAt is after the node's
// LastHeartbeatAt, so a VM created during the test (now) reduces this node's
// effective availability once it is bound. Capacity is sized comfortably larger
// than two small VMs so the scorer - not a capacity limit - steers the spread.
func seedReadyNodeWithPool(t *testing.T, s *etcdstore.Store, cli *etcd.Client, name string, cpu int32, mem int64) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	past := time.Now().UTC().Add(-time.Hour)
	cpuVal := cpu
	memVal := mem
	id := uuid.New()
	n := store.Node{
		ID:                 id,
		Name:               name,
		Architecture:       store.CpuArchAmd64,
		AdvertisedEndpoint: "https://spread.example:9443",
		MigrationHost:      "10.0.0.9",
		Status:             store.NodeStatusReady,
		CPUCoresTotal:      &cpuVal,
		CPUCoresAvailable:  &cpuVal,
		MemoryTotalMib:     &memVal,
		MemoryAvailableMib: &memVal,
		LastHeartbeatAt:    &past,
		CreatedAt:          time.Now().UTC(),
		UpdatedAt:          time.Now().UTC(),
	}
	if err := cli.PutJSON(ctx, etcd.Key("nodes", id.String()), n); err != nil {
		t.Fatalf("seed node %q: %v", name, err)
	}
	if _, err := s.CreateStoragePool(ctx, poolParams(id, "default")); err != nil {
		t.Fatalf("create default pool on %q: %v", name, err)
	}
	return id
}

// mustPinnedNode loads the VM and returns its pinned node id, failing if the VM
// was never scheduled (PinnedNodeID still nil).
func mustPinnedNode(t *testing.T, s *etcdstore.Store, ctx context.Context, vmID uuid.UUID, label string) uuid.UUID {
	t.Helper()
	vm, err := s.VMByID(ctx, vmID)
	if err != nil {
		t.Fatalf("VMByID(%s): %v", label, err)
	}
	if vm.PinnedNodeID == nil {
		t.Fatalf("%s not scheduled: PinnedNodeID is nil", label)
	}
	return *vm.PinnedNodeID
}

// TestPlacementSpreadsAcrossNodes proves that with the placement lock serializing
// the read->commit window, two VMs scheduled in sequence onto an empty 2-node
// cluster land on DIFFERENT nodes: the second placement reads the first's
// committed pin and the LeastAllocated scorer avoids the now-busier node. The
// spread assertion IS the deterministic teeth - the scorer can only avoid the
// first node by observing its pin. It also proves the lock does not deadlock the
// second bind (a missing release would hang the tick). Concurrency correctness
// (overlapping windows) is proven separately by the locker unit test and the
// node-drain smoke, not here.
func TestPlacementSpreadsAcrossNodes(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()

	// Two ready nodes, each with an equal "default" pool, ample for >2 VMs
	// (16 cpu / 32 GiB vs two 2-cpu / 2 GiB VMs): capacity never forces the
	// spread, the scorer does.
	nodeA := seedReadyNodeWithPool(t, s, cli, "node-a", 16, 32768)
	nodeB := seedReadyNodeWithPool(t, s, cli, "node-b", 16, 32768)

	// Two unscheduled VMs requesting small resources that fit on either node.
	pA := mkUnscheduledParams(t, "vm-a")
	if _, err := s.CreateUnscheduledVM(ctx, pA); err != nil {
		t.Fatalf("CreateUnscheduledVM(vm-a): %v", err)
	}
	pB := mkUnscheduledParams(t, "vm-b")
	if _, err := s.CreateUnscheduledVM(ctx, pB); err != nil {
		t.Fatalf("CreateUnscheduledVM(vm-b): %v", err)
	}

	// resource_aware scoring must be active for the spread to be the scorer's
	// doing: enable every resource at ratio 1.0 (the production default the
	// LeastAllocated scorer runs under). A zero ResourcesConfig disables scoring
	// and degrades to count-based, which the test must not rely on.
	res := scheduler.ResourcesConfig{
		CPU:    scheduler.ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
		Memory: scheduler.MemoryResourceConfig{Enabled: true, OvercommitRatio: 1.0},
		Disk:   scheduler.ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	tick := vms.ScheduleFunc(s, vms.ScheduleConfig{Algorithm: scheduler.AlgorithmResourceAware}, log, res)
	if err := tick(ctx); err != nil {
		t.Fatalf("ScheduleFunc tick: %v", err)
	}

	pinA := mustPinnedNode(t, s, ctx, pA.ID, "vm-a")
	pinB := mustPinnedNode(t, s, ctx, pB.ID, "vm-b")
	if pinA == pinB {
		t.Errorf("both VMs pinned to node %s, want spread across node-a (%s) and node-b (%s)", pinA, nodeA, nodeB)
	}
}
