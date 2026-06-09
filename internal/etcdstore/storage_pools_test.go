// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

func poolParams(nodeID uuid.UUID, name string) store.CreateStoragePoolParams {
	return store.CreateStoragePoolParams{
		ID:     uuid.New(),
		NodeID: nodeID,
		Name:   name,
		Type:   "local_dir",
		Path:   "/var/lib/otherix/pools/" + name,
		Config: []byte(`{}`),
	}
}

func uniquePoolName(prefix string) string { return prefix + "-" + uuid.NewString()[:8] }

func TestStoragePoolMultiInstanceAndUniqueness(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	nodeA, nodeB := uuid.New(), uuid.New()
	name := uniquePoolName("pool")
	a, err := s.CreateStoragePool(ctx, poolParams(nodeA, name))
	if err != nil {
		t.Fatalf("CreateStoragePool A: %v", err)
	}
	// Same name on a different node is allowed (multi-instance).
	b, err := s.CreateStoragePool(ctx, poolParams(nodeB, name))
	if err != nil {
		t.Fatalf("CreateStoragePool B: %v", err)
	}
	// Same name on the same node collides.
	if _, err := s.CreateStoragePool(ctx, poolParams(nodeA, name)); !errors.Is(err, store.ErrStoragePoolNameExists) {
		t.Errorf("dup on node = %v, want store.ErrStoragePoolNameExists", err)
	}
	// Name aggregation returns both instances.
	byName, err := s.StoragePoolsByName(ctx, name)
	if err != nil {
		t.Fatalf("StoragePoolsByName: %v", err)
	}
	if len(byName) != 2 {
		t.Errorf("StoragePoolsByName len = %d, want 2", len(byName))
	}
	if _, err := s.StoragePoolByID(ctx, a.ID); err != nil {
		t.Errorf("StoragePoolByID(A): %v", err)
	}
	_ = b
}

func TestStoragePoolByIDNotFoundAndUpdateRename(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	if _, err := s.StoragePoolByID(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("StoragePoolByID(absent) = %v, want store.ErrNotFound", err)
	}
	node := uuid.New()
	p := poolParams(node, uniquePoolName("upd"))
	if _, err := s.CreateStoragePool(ctx, p); err != nil {
		t.Fatalf("CreateStoragePool: %v", err)
	}
	newName := uniquePoolName("upd-renamed")
	updated, err := s.UpdateStoragePool(ctx, store.UpdateStoragePoolParams{ID: p.ID, Name: newName, Config: []byte(`{"a":1}`)})
	if err != nil {
		t.Fatalf("UpdateStoragePool: %v", err)
	}
	if updated.Name != newName {
		t.Errorf("UpdateStoragePool name = %q, want %q", updated.Name, newName)
	}
	// Old name aggregation is now empty; new name resolves.
	if got, _ := s.StoragePoolsByName(ctx, p.Name); len(got) != 0 {
		t.Errorf("old name still resolves: %d instances", len(got))
	}
	if got, _ := s.StoragePoolsByName(ctx, newName); len(got) != 1 {
		t.Errorf("new name instances = %d, want 1", len(got))
	}
}

func TestPoolEffectiveCapacityPending(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	// Seed a pool directly with reported availability + an old scan time.
	scan := time.Now().UTC().Add(-time.Hour)
	avail := int64(100 * bytesPerGiBTest)
	poolID := uuid.New()
	p := store.StoragePool{
		ID: poolID, NodeID: uuid.New(), Name: uniquePoolName("eff"), Type: "local_dir",
		Path: "/var/lib/otherix/pools/eff", AvailableBytes: &avail, ReportedAt: &scan,
		ReconciliationStatus: "ok", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := cli.PutJSON(ctx, etcd.Key("storage_pools", poolID.String()), p); err != nil {
		t.Fatalf("seed pool: %v", err)
	}
	// A vm_disk committed after the scan -> pending 10 GiB.
	diskID := uuid.New()
	d := store.VMDisk{ID: diskID, StoragePoolID: poolID, SizeGib: 10, CreatedAt: time.Now().UTC()}
	if err := cli.PutJSON(ctx, etcd.Key("vm_disks", diskID.String()), d); err != nil {
		t.Fatalf("seed disk: %v", err)
	}
	if err := cli.Put(ctx, etcd.Key("index", "vm_disks", "pool", poolID.String(), diskID.String()), []byte(diskID.String())); err != nil {
		t.Fatalf("seed disk index: %v", err)
	}
	eff, err := s.PoolEffectiveByID(ctx, poolID)
	if err != nil {
		t.Fatalf("PoolEffectiveByID: %v", err)
	}
	want := int64(90 * bytesPerGiBTest)
	if eff.AvailableBytesEffective == nil || *eff.AvailableBytesEffective != want {
		t.Errorf("AvailableBytesEffective = %v, want %d", eff.AvailableBytesEffective, want)
	}
}

func TestPoolImageInventoryRoundTrip(t *testing.T) {
	st, _ := startStore(t)
	ctx := context.Background()
	poolID := uuid.New()
	want := []store.PoolImage{{
		Basename: "ubuntu-24.04-arm64.img", ChecksumSha256: "ab12", SizeBytes: 100,
		VirtualSizeBytes: 200, Format: "qcow2", ImportedAt: time.Now().UTC().Truncate(time.Second),
	}}
	if err := st.UpsertPoolImageInventory(ctx, poolID, want); err != nil {
		t.Fatalf("UpsertPoolImageInventory() error = %v", err)
	}
	got, err := st.PoolImageInventory(ctx, poolID)
	if err != nil {
		t.Fatalf("PoolImageInventory() error = %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("PoolImageInventory() mismatch (-want +got):\n%s", diff)
	}

	// An empty slice clears the inventory: a pool that dropped all images
	// reports empty, not stale.
	if err := st.UpsertPoolImageInventory(ctx, poolID, nil); err != nil {
		t.Fatalf("UpsertPoolImageInventory(empty) error = %v", err)
	}
	cleared, err := st.PoolImageInventory(ctx, poolID)
	if err != nil {
		t.Fatalf("PoolImageInventory(after clear) error = %v", err)
	}
	if len(cleared) != 0 {
		t.Errorf("PoolImageInventory(after clear) = %v, want empty", cleared)
	}
}

// bytesPerGiBTest mirrors the package-internal bytesPerGiB constant for the
// external test package.
const bytesPerGiBTest = 1073741824

var _ = etcdstore.New
