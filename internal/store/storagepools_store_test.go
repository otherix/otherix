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

// The tests in this file exercise the storage-pool and storage-image
// domain methods on *Store: the name-uniqueness translation, the
// transactional DeleteStoragePool blocking path, and the
// refcount-gated DeleteStorageImageRefcounted seam (callback invoked
// only on the last referent). Raw sqlc query behaviour is covered in
// storage_pools_test.go / storage_images_test.go.

// seedImage inserts a storage_image for (tpl, pool) with the given
// checksum and returns its id.
func seedImage(t *testing.T, ctx context.Context, s *store.Store, tpl, pool uuid.UUID, sha string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := s.Queries().CreateStorageImage(ctx, store.CreateStorageImageParams{
		ID:             id,
		TemplateID:     tpl,
		PoolID:         pool,
		ChecksumSha256: sha,
		SizeBytes:      4096,
		Format:         "qcow2",
	}); err != nil {
		t.Fatalf("seed image: %v", err)
	}
	return id
}

func TestCreateStoragePoolDuplicateName(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	node := seedNodeForPools(t, ctx, s)
	name := uniquePoolName("dom-dup")
	if _, err := s.CreateStoragePool(ctx, defaultPoolParams(uuid.New(), node, name)); err != nil {
		t.Fatalf("first CreateStoragePool: %v", err)
	}
	_, err := s.CreateStoragePool(ctx, defaultPoolParams(uuid.New(), node, name))
	if !errors.Is(err, store.ErrStoragePoolNameExists) {
		t.Errorf("duplicate name err = %v, want store.ErrStoragePoolNameExists", err)
	}
}

func TestUpdateStoragePoolDuplicateName(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	node := seedNodeForPools(t, ctx, s)
	taken := uniquePoolName("dom-taken")
	if _, err := s.CreateStoragePool(ctx, defaultPoolParams(uuid.New(), node, taken)); err != nil {
		t.Fatalf("seed taken: %v", err)
	}
	moverID := uuid.New()
	if _, err := s.CreateStoragePool(ctx, defaultPoolParams(moverID, node, uniquePoolName("dom-mover"))); err != nil {
		t.Fatalf("seed mover: %v", err)
	}

	_, err := s.UpdateStoragePool(ctx, store.UpdateStoragePoolParams{
		ID:     moverID,
		Name:   taken,
		Config: []byte(`{}`),
	})
	if !errors.Is(err, store.ErrStoragePoolNameExists) {
		t.Errorf("rename collision err = %v, want store.ErrStoragePoolNameExists", err)
	}
}

func TestDeleteStoragePoolSucceeds(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	node := seedNodeForPools(t, ctx, s)
	id := seedPoolForImages(t, ctx, s, node, "dom-del")
	if err := s.DeleteStoragePool(ctx, id); err != nil {
		t.Fatalf("DeleteStoragePool: %v", err)
	}
	if _, err := s.Queries().GetStoragePoolByID(ctx, id); err == nil {
		t.Errorf("GetStoragePoolByID after delete = nil error, want not-found")
	}
}

func TestDeleteStoragePoolBlockedByImage(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	owner := seedUser(t, ctx, s, "developer")
	node := seedNodeForPools(t, ctx, s)
	tpl := seedTemplateForImages(t, ctx, s, owner, "dom-blk")
	pool := seedPoolForImages(t, ctx, s, node, "dom-blk")
	seedImage(t, ctx, s, tpl, pool, imageSHA256(0x11))

	err := s.DeleteStoragePool(ctx, pool)
	var blocking *store.ResourceInUseError
	if !errors.As(err, &blocking) {
		t.Fatalf("DeleteStoragePool err = %v, want *store.ResourceInUseError", err)
	}
	if blocking.Resources["storage_images"] != 1 {
		t.Errorf("blocking storage_images = %d, want 1", blocking.Resources["storage_images"])
	}
}

func TestStorageImageByIDNotFound(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	if _, err := s.StorageImageByID(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("StorageImageByID(absent) err = %v, want store.ErrNotFound", err)
	}
}

func TestTemplateByIDNotFound(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	if _, err := s.TemplateByID(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("TemplateByID(absent) err = %v, want store.ErrNotFound", err)
	}
}

func TestDeleteStorageImageRefcountedLastReferent(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	owner := seedUser(t, ctx, s, "developer")
	node := seedNodeForPools(t, ctx, s)
	tpl := seedTemplateForImages(t, ctx, s, owner, "dom-ref1")
	pool := seedPoolForImages(t, ctx, s, node, "dom-ref1")
	img := seedImage(t, ctx, s, tpl, pool, imageSHA256(0x22))

	called := false
	gotImage, gotTpl, err := s.DeleteStorageImageRefcounted(ctx, pool, img,
		func(store.Template) error { return nil },
		func(_ context.Context, n store.Node, poolName, checksum string) error {
			called = true
			if n.ID != node {
				t.Errorf("onLastReferent node = %v, want %v", n.ID, node)
			}
			if checksum != imageSHA256(0x22) {
				t.Errorf("onLastReferent checksum = %q, want %q", checksum, imageSHA256(0x22))
			}
			return nil
		})
	if err != nil {
		t.Fatalf("DeleteStorageImageRefcounted: %v", err)
	}
	if !called {
		t.Error("onLastReferent was not called for the last referent")
	}
	if gotImage.ID != img {
		t.Errorf("returned image.ID = %v, want %v", gotImage.ID, img)
	}
	if gotTpl.ID != tpl {
		t.Errorf("returned template.ID = %v, want %v", gotTpl.ID, tpl)
	}
	if _, err := s.Queries().GetStorageImageByID(ctx, img); err == nil {
		t.Error("image row still present after delete")
	}
}

func TestDeleteStorageImageRefcountedSiblingSkipsCallback(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	owner := seedUser(t, ctx, s, "developer")
	node := seedNodeForPools(t, ctx, s)
	tplA := seedTemplateForImages(t, ctx, s, owner, "dom-ref2a")
	tplB := seedTemplateForImages(t, ctx, s, owner, "dom-ref2b")
	pool := seedPoolForImages(t, ctx, s, node, "dom-ref2")
	sha := imageSHA256(0x33)
	target := seedImage(t, ctx, s, tplA, pool, sha)
	seedImage(t, ctx, s, tplB, pool, sha) // sibling sharing the checksum

	called := false
	_, _, err := s.DeleteStorageImageRefcounted(ctx, pool, target,
		func(store.Template) error { return nil },
		func(context.Context, store.Node, string, string) error {
			called = true
			return nil
		})
	if err != nil {
		t.Fatalf("DeleteStorageImageRefcounted: %v", err)
	}
	if called {
		t.Error("onLastReferent invoked despite a surviving sibling sharing the checksum")
	}
}

func TestDeleteStorageImageRefcountedOnLastReferentErrorRollsBack(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	owner := seedUser(t, ctx, s, "developer")
	node := seedNodeForPools(t, ctx, s)
	tpl := seedTemplateForImages(t, ctx, s, owner, "dom-ref6")
	pool := seedPoolForImages(t, ctx, s, node, "dom-ref6")
	img := seedImage(t, ctx, s, tpl, pool, imageSHA256(0x66))

	// The agent file-delete is the whole reason the delete runs inside a
	// transaction: when onLastReferent fails the row must NOT be removed.
	agentErr := errors.New("agent unreachable")
	_, _, err := s.DeleteStorageImageRefcounted(ctx, pool, img,
		func(store.Template) error { return nil },
		func(context.Context, store.Node, string, string) error { return agentErr })
	if !errors.Is(err, agentErr) {
		t.Fatalf("DeleteStorageImageRefcounted err = %v, want agentErr", err)
	}
	if _, err := s.Queries().GetStorageImageByID(ctx, img); err != nil {
		t.Errorf("image row removed despite onLastReferent failure: %v", err)
	}
}

func TestDeleteStorageImageRefcountedNotFound(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	node := seedNodeForPools(t, ctx, s)
	pool := seedPoolForImages(t, ctx, s, node, "dom-ref3")

	_, _, err := s.DeleteStorageImageRefcounted(ctx, pool, uuid.New(),
		func(store.Template) error { return nil },
		func(context.Context, store.Node, string, string) error { return nil })
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing image err = %v, want store.ErrNotFound", err)
	}
}

func TestDeleteStorageImageRefcountedWrongPool(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	owner := seedUser(t, ctx, s, "developer")
	node := seedNodeForPools(t, ctx, s)
	tpl := seedTemplateForImages(t, ctx, s, owner, "dom-ref4")
	poolA := seedPoolForImages(t, ctx, s, node, "dom-ref4a")
	poolB := seedPoolForImages(t, ctx, s, node, "dom-ref4b")
	img := seedImage(t, ctx, s, tpl, poolA, imageSHA256(0x44))

	// Address the image through the wrong pool -> ErrNotFound (no leak).
	_, _, err := s.DeleteStorageImageRefcounted(ctx, poolB, img,
		func(store.Template) error { return nil },
		func(context.Context, store.Node, string, string) error { return nil })
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("wrong-pool image err = %v, want store.ErrNotFound", err)
	}
}

func TestDeleteStorageImageRefcountedAuthorizeRejected(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	owner := seedUser(t, ctx, s, "developer")
	node := seedNodeForPools(t, ctx, s)
	tpl := seedTemplateForImages(t, ctx, s, owner, "dom-ref5")
	pool := seedPoolForImages(t, ctx, s, node, "dom-ref5")
	img := seedImage(t, ctx, s, tpl, pool, imageSHA256(0x55))

	sentinel := errors.New("forbidden")
	_, _, err := s.DeleteStorageImageRefcounted(ctx, pool, img,
		func(store.Template) error { return sentinel },
		func(context.Context, store.Node, string, string) error { return nil })
	if !errors.Is(err, sentinel) {
		t.Errorf("authorize-reject err = %v, want sentinel", err)
	}
	// The row must survive a rejected authorization.
	if _, err := s.Queries().GetStorageImageByID(ctx, img); err != nil {
		t.Errorf("image row removed despite authorize rejection: %v", err)
	}
}
