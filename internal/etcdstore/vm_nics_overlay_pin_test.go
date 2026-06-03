// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// seedNICForVM writes one more overlay NIC onto an existing VM: the NIC row plus
// its per-network delete-block index entry, the two keys the placement
// projection reads. Both NICs share the VM, so both resolve through the SAME
// vm_runtime key - the join must read that key once per snapshot, not once per
// NIC at drifting revisions.
func seedNICForVM(t *testing.T, cli *etcd.Client, vmID, networkID uuid.UUID, mac string) {
	t.Helper()
	ctx := context.Background()
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
		t.Fatalf("seedNICForVM: put nic: %v", err)
	}
	idxKey := etcd.Key("index", "vm_nics", "network", networkID.String(), nicID.String())
	if err := cli.Put(ctx, idxKey, []byte(nicID.String())); err != nil {
		t.Fatalf("seedNICForVM: put network index: %v", err)
	}
}

// TestListOverlayNICPlacementsPinnedSnapshotConsistency drives a concurrent
// runtime node-flip on a single VM that owns two overlay NICs while repeatedly
// projecting placements. Both NICs join through the SAME vm_runtime key, so a
// consistent snapshot must report BOTH NICs on the same node. On the pre-fix
// (unpinned) code each per-NIC runtime read sees an independent MVCC revision,
// so a flip landing between the two reads yields one NIC on nodeA and the other
// on nodeB - a torn read. The pinned join reads every row at one revision and
// the two NICs always agree.
func TestListOverlayNICPlacementsPinnedSnapshotConsistency(t *testing.T) {
	ctx := context.Background()
	s, cli := startStore(t)

	nodeA, nodeB := uuid.New(), uuid.New()
	ov := seedOverlayNetwork(t, s)
	vm := seedVMWithNIC(t, cli, ov.ID, "52:54:00:00:00:0a")
	seedNICForVM(t, cli, vm, ov.ID, "52:54:00:00:00:0b")
	placeVM(t, cli, vm, nodeA)

	const flips = 4000
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < flips; i++ {
			node := nodeA
			if i%2 == 1 {
				node = nodeB
			}
			placeVM(t, cli, vm, node)
		}
	}()

	var torn int
	for i := 0; i < flips; i++ {
		got, _, err := s.ListOverlayNICPlacementsPinned(ctx)
		if err != nil {
			t.Fatalf("ListOverlayNICPlacementsPinned: %v", err)
		}
		// Both NICs belong to one VM, so within one snapshot every placement
		// must name one node. More than one distinct node => torn read.
		nodes := make(map[uuid.UUID]struct{}, 2)
		for _, p := range got {
			nodes[p.NodeID] = struct{}{}
		}
		if len(nodes) > 1 {
			torn++
		}
	}
	wg.Wait()

	if torn != 0 {
		t.Errorf("observed %d torn placement snapshots (two NICs of one VM on different nodes); the join is not revision-pinned", torn)
	}
}
