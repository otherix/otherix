// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// startInternalStore returns a Store over the shared embedded member with a
// freshly wiped keyspace (see FreshStore / TestMain in main_test.go). It lives
// in the internal test package so tests can reach the unexported delete-ops
// builder and index-key helpers directly.
func startInternalStore(t *testing.T) *Store {
	t.Helper()
	s, _ := FreshStore(t)
	return s
}

// TestVMNicDeleteOpsTombstoneDropsNetworkIndex reproduces the redelivery state -
// a NIC row already soft-deleted while both its per-VM and per-network index
// entries linger - and verifies vmNicDeleteOps drops BOTH indexes. Without the
// per-network drop a future NIC-soft-delete path would leave that index and
// wedge DeleteNetwork forever.
func TestVMNicDeleteOpsTombstoneDropsNetworkIndex(t *testing.T) {
	s := startInternalStore(t)
	ctx := context.Background()

	vmID := uuid.New()
	nicID := uuid.New()
	netID := uuid.New()
	deletedAt := time.Now().UTC()

	// Seed the redelivery state: a soft-deleted NIC row plus both index entries.
	nic := store.VMNic{
		ID: nicID, VmID: vmID, NetworkID: netID, DeviceOrder: 0,
		Model: store.NicModelVirtio, Generation: 1,
		CreatedAt: deletedAt, UpdatedAt: deletedAt, DeletedAt: &deletedAt,
	}
	if err := s.c.PutJSON(ctx, vmNicKey(nicID), nic); err != nil {
		t.Fatalf("seed nic row: %v", err)
	}
	if err := s.c.Put(ctx, vmNicVMIndexKey(vmID, nicID), []byte(nicID.String())); err != nil {
		t.Fatalf("seed vm index: %v", err)
	}
	if err := s.c.Put(ctx, vmNicNetworkIndexKey(netID, nicID), []byte(nicID.String())); err != nil {
		t.Fatalf("seed network index: %v", err)
	}

	ops, err := s.vmNicDeleteOps(ctx, vmID, time.Now().UTC())
	if err != nil {
		t.Fatalf("vmNicDeleteOps: %v", err)
	}
	if _, err := s.c.Raw().Txn(ctx).Then(ops...).Commit(); err != nil {
		t.Fatalf("commit delete ops: %v", err)
	}

	// Both indexes must be gone after the tombstone branch ran.
	if _, found, err := s.c.Get(ctx, vmNicVMIndexKey(vmID, nicID)); err != nil || found {
		t.Errorf("per-VM index still present (found=%v err=%v), want dropped", found, err)
	}
	if _, found, err := s.c.Get(ctx, vmNicNetworkIndexKey(netID, nicID)); err != nil || found {
		t.Errorf("per-network index still present (found=%v err=%v), want dropped", found, err)
	}
}
