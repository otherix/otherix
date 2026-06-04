// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// startInternalStore boots a single-node embedded member and returns a Store
// over it. It lives in the internal test package so tests can reach the
// unexported delete-ops builder and index-key helpers directly.
func startInternalStore(t *testing.T) *Store {
	t.Helper()
	cfg := &etcd.Config{
		Mode:         etcd.ModeSingle,
		Name:         "n1",
		DataDir:      filepath.Join(t.TempDir(), "member"),
		PeerURL:      fmt.Sprintf("http://127.0.0.1:%d", internalFreePort(t)),
		ClientURL:    fmt.Sprintf("http://127.0.0.1:%d", internalFreePort(t)),
		ClusterToken: "otherix-test",
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := etcd.Start(ctx, cfg, log)
	if err != nil {
		t.Fatalf("etcd.Start: %v", err)
	}
	cli := etcd.NewClient(r)
	t.Cleanup(func() {
		_ = cli.Close()
		r.Stop(10 * time.Second)
	})
	return New(cli)
}

func internalFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
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
