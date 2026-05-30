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
		Path:   "/opt/otherix/pools/" + name,
		Config: []byte(`{}`),
	}
}

func uniquePoolName(prefix string) string { return prefix + "-" + uuid.NewString()[:8] }

// seedImage writes a storage image plus its pool + template indexes directly
// (images are created by the import worker, not a handler Store method).
func seedImage(t *testing.T, cli *etcd.Client, poolID, templateID uuid.UUID, checksum string) store.StorageImage {
	t.Helper()
	ctx := context.Background()
	im := store.StorageImage{
		ID: uuid.New(), TemplateID: templateID, PoolID: poolID,
		ChecksumSha256: checksum, SizeBytes: 1 << 30, Format: "qcow2", ImportedAt: time.Now().UTC(),
	}
	if err := cli.PutJSON(ctx, etcd.Key("storage_images", im.ID.String()), im); err != nil {
		t.Fatalf("seed image: %v", err)
	}
	if err := cli.Put(ctx, etcd.Key("index", "storage_images", "pool", poolID.String(), im.ID.String()), []byte(im.ID.String())); err != nil {
		t.Fatalf("seed image pool index: %v", err)
	}
	if err := cli.Put(ctx, etcd.Key("index", "storage_images", "template", templateID.String(), im.ID.String()), []byte(im.ID.String())); err != nil {
		t.Fatalf("seed image template index: %v", err)
	}
	return im
}

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
		Path: "/opt/otherix/pools/eff", AvailableBytes: &avail, ReportedAt: &scan,
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

func TestStoragePoolDeleteBlockingByImage(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	node := uuid.New()
	p := poolParams(node, uniquePoolName("del"))
	if _, err := s.CreateStoragePool(ctx, p); err != nil {
		t.Fatalf("CreateStoragePool: %v", err)
	}
	seedImage(t, cli, p.ID, uuid.New(), "deadbeef")
	var blocking *store.ResourceInUseError
	if err := s.DeleteStoragePool(ctx, p.ID); !errors.As(err, &blocking) || blocking.Resources["storage_images"] != 1 {
		t.Fatalf("DeleteStoragePool blocked = %v, want storage_images=1", err)
	}
}

func TestStorageImagesListAndRefcountedDelete(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	node := nodeParams(uniqueNodeName("img"))
	if _, err := s.CreateNode(ctx, node); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	p := poolParams(node.ID, uniquePoolName("img"))
	if _, err := s.CreateStoragePool(ctx, p); err != nil {
		t.Fatalf("CreateStoragePool: %v", err)
	}
	tpl := tplParams(uniqueTplName("img"), uuid.New())
	if _, err := s.CreateTemplate(ctx, tpl); err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	// Two images share a checksum (dedup), one distinct.
	shared1 := seedImage(t, cli, p.ID, tpl.ID, "sharedsum")
	shared2 := seedImage(t, cli, p.ID, tpl.ID, "sharedsum")
	solo := seedImage(t, cli, p.ID, tpl.ID, "solosum")

	list, err := s.ListStorageImagesByPool(ctx, store.ListStorageImagesByPoolParams{PoolID: p.ID, LimitCount: 200})
	if err != nil {
		t.Fatalf("ListStorageImagesByPool: %v", err)
	}
	if len(list) != 3 {
		t.Errorf("images len = %d, want 3", len(list))
	}
	for i := 1; i < len(list); i++ {
		if list[i].ImportedAt.After(list[i-1].ImportedAt) {
			t.Errorf("images not descending at %d", i)
		}
	}

	// Deleting one of the shared pair must NOT fire onLastReferent.
	var fired bool
	if _, err := s.StorageImageByID(ctx, shared1.ID); err != nil {
		t.Fatalf("pre-check: %v", err)
	}
	if _, _, err := s.DeleteStorageImageRefcounted(ctx, p.ID, shared1.ID,
		func(store.Template) error { return nil },
		func(context.Context, store.Node, string, string) error { fired = true; return nil },
	); err != nil {
		t.Fatalf("delete shared1: %v", err)
	}
	if fired {
		t.Errorf("onLastReferent fired while a sibling shares the checksum")
	}
	// Deleting the last sibling fires it.
	fired = false
	if _, _, err := s.DeleteStorageImageRefcounted(ctx, p.ID, shared2.ID,
		func(store.Template) error { return nil },
		func(context.Context, store.Node, string, string) error { fired = true; return nil },
	); err != nil {
		t.Fatalf("delete shared2: %v", err)
	}
	if !fired {
		t.Errorf("onLastReferent did not fire for the last referent")
	}
	// Wrong pool -> not found; authorize error propagates.
	if _, _, err := s.DeleteStorageImageRefcounted(ctx, uuid.New(), solo.ID,
		func(store.Template) error { return nil },
		func(context.Context, store.Node, string, string) error { return nil },
	); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("wrong-pool delete = %v, want store.ErrNotFound", err)
	}
	sentinel := errors.New("denied")
	if _, _, err := s.DeleteStorageImageRefcounted(ctx, p.ID, solo.ID,
		func(store.Template) error { return sentinel },
		func(context.Context, store.Node, string, string) error { return nil },
	); !errors.Is(err, sentinel) {
		t.Errorf("authorize error = %v, want propagated sentinel", err)
	}
}

// bytesPerGiBTest mirrors the package-internal bytesPerGiB constant for the
// external test package.
const bytesPerGiBTest = 1073741824

var _ = etcdstore.New
