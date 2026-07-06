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
	"github.com/otherix/otherix/internal/store"
)

// runtimeRowExists reports whether the vm_runtime row for vmID is present.
func runtimeRowExists(t *testing.T, cli *etcd.Client, vmID uuid.UUID) bool {
	t.Helper()
	_, found, err := cli.Get(context.Background(), etcd.Key("vm_runtime", vmID.String()))
	if err != nil {
		t.Fatalf("get vm_runtime: %v", err)
	}
	return found
}

// TestUpsertVMRuntimeCASSkipsStaleRev is the CAS teeth for the heartbeat runtime
// write: UpsertVMRuntime commits under a compare on the vms row ModRevision the
// heartbeat read, so a write carrying a stale rev (the row moved under it - a
// delete or a migration cutover) is skipped, not applied. A zero rev means "no
// CAS" (unconditional), preserving callers that do not participate in the fence.
func TestUpsertVMRuntimeCASSkipsStaleRev(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()

	vm := vmRow(uniqueNodeName("casvm"))
	seedVM(t, cli, vm)
	node := uuid.New()

	// Read the rev the heartbeat would decide against.
	var staleRev int64
	if err := s.RunHeartbeatProjection(ctx, func(hp store.HeartbeatProjection) error {
		_, rev, err := hp.VMWithRev(ctx, vm.ID)
		if err != nil {
			return err
		}
		staleRev = rev
		return nil
	}); err != nil {
		t.Fatalf("read rev: %v", err)
	}

	// The vms row moves under the heartbeat (stand-in for a delete/cutover write):
	// re-put bumps its ModRevision, so staleRev no longer matches.
	if err := cli.PutJSON(ctx, etcd.Key("vms", vm.ID.String()), vm); err != nil {
		t.Fatalf("bump vms row: %v", err)
	}

	// A write carrying the now-stale rev is skipped: no runtime row appears.
	if err := s.RunHeartbeatProjection(ctx, func(hp store.HeartbeatProjection) error {
		return hp.UpsertVMRuntime(ctx, store.UpsertVMRuntimeParams{
			VmID: vm.ID, CurrentNodeID: &node, Phase: store.VmPhaseRunning,
			ObservedGeneration: 1, VMRowModRevision: staleRev,
		})
	}); err != nil {
		t.Fatalf("stale upsert: %v", err)
	}
	if runtimeRowExists(t, cli, vm.ID) {
		t.Fatalf("stale-rev UpsertVMRuntime resurrected/created the runtime row; want skipped")
	}

	// A write carrying the fresh rev commits.
	var freshRev int64
	if err := s.RunHeartbeatProjection(ctx, func(hp store.HeartbeatProjection) error {
		_, rev, err := hp.VMWithRev(ctx, vm.ID)
		if err != nil {
			return err
		}
		freshRev = rev
		return hp.UpsertVMRuntime(ctx, store.UpsertVMRuntimeParams{
			VmID: vm.ID, CurrentNodeID: &node, Phase: store.VmPhaseRunning,
			ObservedGeneration: 1, VMRowModRevision: rev,
		})
	}); err != nil {
		t.Fatalf("fresh upsert: %v", err)
	}
	if freshRev == staleRev {
		t.Fatalf("freshRev == staleRev (%d); the row bump did not change the rev, test is toothless", freshRev)
	}
	if !runtimeRowExists(t, cli, vm.ID) {
		t.Fatalf("fresh-rev UpsertVMRuntime did not write the runtime row")
	}
}

// TestUpsertVMRuntimeZeroRevWritesUnconditionally documents the sentinel: a zero
// VMRowModRevision skips the compare so non-fence callers (and the existing test
// call sites) keep their unconditional-write behavior.
func TestUpsertVMRuntimeZeroRevWritesUnconditionally(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()

	vm := vmRow(uniqueNodeName("casvm0"))
	seedVM(t, cli, vm)
	node := uuid.New()

	if err := s.RunHeartbeatProjection(ctx, func(hp store.HeartbeatProjection) error {
		return hp.UpsertVMRuntime(ctx, store.UpsertVMRuntimeParams{
			VmID: vm.ID, CurrentNodeID: &node, Phase: store.VmPhaseRunning, ObservedGeneration: 1,
		})
	}); err != nil {
		t.Fatalf("zero-rev upsert: %v", err)
	}
	if !runtimeRowExists(t, cli, vm.ID) {
		t.Fatalf("zero-rev UpsertVMRuntime did not write the runtime row")
	}
}
