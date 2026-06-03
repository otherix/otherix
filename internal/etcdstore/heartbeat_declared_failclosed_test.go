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

// seedRuntimeBoundToNode writes the vm_runtime row plus the vm_runtime-by-node
// index entry the heartbeat declared-set join ranges, the way reindexRuntimeNode
// does on first bind. It does NOT write the vms primary row, so callers can
// control whether the primary read is a clean hit, a genuine ErrNotFound, or a
// corrupt (transient-error analog) read.
func seedRuntimeBoundToNode(t *testing.T, cli *etcd.Client, vmID, nodeID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	rt := store.VMRuntime{VmID: vmID, CurrentNodeID: &nodeID, Phase: store.VmPhaseRunning, ObservedGeneration: 1}
	if err := cli.PutJSON(ctx, etcd.Key("vm_runtime", vmID.String()), rt); err != nil {
		t.Fatalf("seedRuntimeBoundToNode: put runtime: %v", err)
	}
	if err := cli.Put(ctx, etcd.Key("index", "vm_runtime", "node", nodeID.String(), vmID.String()), []byte(vmID.String())); err != nil {
		t.Fatalf("seedRuntimeBoundToNode: put node index: %v", err)
	}
}

// TestListVMsForNodeDeclaredFailsClosedOnTransientRead asserts the declared-set
// join FAILS CLOSED: a non-ErrNotFound primary read error aborts the whole load
// rather than silently omitting the VM from an otherwise-successful set. A
// partial set would be decoded by the agent as an authoritative desired set and
// could drive the absence-debounce to prune a live VM the CP still wants. A
// corrupt vms primary row stands in for a transient read failure: GetJSON
// returns an unmarshal error (a non-ErrNotFound error), the exact class the join
// must abort on.
func TestListVMsForNodeDeclaredFailsClosedOnTransientRead(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()

	nodeID := uuid.New()
	// A healthy live VM bound to the node: index entry + runtime + good primary row.
	live := vmRow(uniqueNodeName("live"))
	seedVM(t, cli, live)
	seedRuntimeBoundToNode(t, cli, live.ID, nodeID)

	// A second VM bound to the same node whose primary row is CORRUPT - the
	// transient-read-error analog. The join must abort, not skip it.
	badID := uuid.New()
	seedRuntimeBoundToNode(t, cli, badID, nodeID)
	if err := cli.Put(ctx, etcd.Key("vms", badID.String()), []byte("{not json")); err != nil {
		t.Fatalf("seed corrupt vms row: %v", err)
	}

	var listErr error
	if err := s.RunHeartbeatProjection(ctx, func(hp store.HeartbeatProjection) error {
		_, listErr = hp.ListVMsForNodeDeclared(ctx, nodeID)
		return nil
	}); err != nil {
		t.Fatalf("RunHeartbeatProjection: %v", err)
	}
	if listErr == nil {
		t.Fatalf("ListVMsForNodeDeclared returned nil error on a transient primary-read failure; it must fail closed (a partial 200 can prune a live VM)")
	}
}

// TestListVMsForNodeDeclaredOmitsGenuinelyDeleted asserts a genuinely-deleted VM
// (vms row absent / soft-deleted -> ErrNotFound, runtime-index entry orphaned or
// atomically deleted) is cleanly OMITTED without erroring. Per the A2 fix the
// runtime-node index entry is deleted atomically with the vms soft-delete, so at
// a single revision an ErrNotFound is a genuine "gone" and cannot drive a wrong
// prune (the VM really is deleted).
func TestListVMsForNodeDeclaredOmitsGenuinelyDeleted(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()

	nodeID := uuid.New()
	live := vmRow(uniqueNodeName("live"))
	seedVM(t, cli, live)
	seedRuntimeBoundToNode(t, cli, live.ID, nodeID)

	// A gone VM: runtime + index entry present (an orphaned/pre-A2 entry), no
	// vms primary row -> VMByID returns ErrNotFound.
	goneID := uuid.New()
	seedRuntimeBoundToNode(t, cli, goneID, nodeID)

	var declared []store.ListVMsForNodeDeclaredRow
	if err := s.RunHeartbeatProjection(ctx, func(hp store.HeartbeatProjection) error {
		var e error
		declared, e = hp.ListVMsForNodeDeclared(ctx, nodeID)
		return e
	}); err != nil {
		t.Fatalf("ListVMsForNodeDeclared: %v", err)
	}
	if len(declared) != 1 || declared[0].Name != live.Name {
		t.Errorf("declared vms = %+v, want [%s] (gone VM cleanly omitted)", declared, live.Name)
	}
}

// TestListVMsForNodeDeclaredPinnedSnapshot drives a concurrent mutation of a
// VM's primary row while repeatedly projecting the declared set. The join is
// revision-pinned: the index range and every per-VM read share one MVCC
// snapshot, so a concurrent vms-row mutation can never manufacture a phantom
// omission of a live, still-wanted VM. The set must always contain the live VM.
func TestListVMsForNodeDeclaredPinnedSnapshot(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()

	nodeID := uuid.New()
	vm := vmRow(uniqueNodeName("pin"))
	seedVM(t, cli, vm)
	seedRuntimeBoundToNode(t, cli, vm.ID, nodeID)

	const rounds = 2000
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			v := vm
			v.Generation = int64(i) + 1
			if err := cli.PutJSON(ctx, etcd.Key("vms", v.ID.String()), v); err != nil {
				return
			}
		}
	}()

	var missing int
	for i := 0; i < rounds; i++ {
		var declared []store.ListVMsForNodeDeclaredRow
		if err := s.RunHeartbeatProjection(ctx, func(hp store.HeartbeatProjection) error {
			var e error
			declared, e = hp.ListVMsForNodeDeclared(ctx, nodeID)
			return e
		}); err != nil {
			t.Fatalf("ListVMsForNodeDeclared: %v", err)
		}
		found := false
		for _, d := range declared {
			if d.Name == vm.Name {
				found = true
			}
		}
		if !found {
			missing++
		}
	}
	wg.Wait()

	if missing != 0 {
		t.Errorf("declared set omitted the live VM in %d/%d projections under concurrent mutation; the join is not revision-pinned", missing, rounds)
	}
}
