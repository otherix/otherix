// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

func TestStorageImageExists(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	_, poolID, templateID, _ := schedulingFixture(t, s)

	exists, err := s.StorageImageExists(ctx, templateID, poolID)
	if err != nil {
		t.Fatalf("StorageImageExists(unknown): %v", err)
	}
	if exists {
		t.Errorf("StorageImageExists(unknown) = true, want false")
	}

	if err := s.EnsureStorageImageProjected(ctx,
		store.CreateStorageImageParams{
			ID: uuid.New(), TemplateID: templateID, PoolID: poolID,
			ChecksumSha256: "ab-checksum", SizeBytes: 1234, Format: "qcow2",
		},
		store.UpdateTemplateImageChecksumIfNullParams{ID: templateID, ImageChecksumSha256: bytes.Repeat([]byte{0xAB}, 32)},
	); err != nil {
		t.Fatalf("EnsureStorageImageProjected: %v", err)
	}

	exists, err = s.StorageImageExists(ctx, templateID, poolID)
	if err != nil {
		t.Fatalf("StorageImageExists(present): %v", err)
	}
	if !exists {
		t.Errorf("StorageImageExists(present) = false, want true")
	}
}

func TestEnsureStorageImageProjected(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	_, poolID, templateID, _ := schedulingFixture(t, s)

	checksum := bytes.Repeat([]byte{0xAB}, 32)
	imageID := uuid.New()
	if err := s.EnsureStorageImageProjected(ctx,
		store.CreateStorageImageParams{
			ID: imageID, TemplateID: templateID, PoolID: poolID,
			ChecksumSha256: "ab-checksum", SizeBytes: 1234, Format: "qcow2",
		},
		store.UpdateTemplateImageChecksumIfNullParams{ID: templateID, ImageChecksumSha256: checksum},
	); err != nil {
		t.Fatalf("EnsureStorageImageProjected: %v", err)
	}

	imgs, err := s.ListStorageImagesByPool(ctx, store.ListStorageImagesByPoolParams{PoolID: poolID, LimitCount: 50})
	if err != nil || len(imgs) != 1 || imgs[0].ID != imageID {
		t.Fatalf("images = (%v, %v), want one with id %v", imgs, err, imageID)
	}
	if imgs[0].ChecksumSha256 != "ab-checksum" || imgs[0].SizeBytes != 1234 || imgs[0].Format != "qcow2" {
		t.Errorf("image = %+v, want ab-checksum/1234/qcow2", imgs[0])
	}

	// Compute-mode template gets the checksum back-propagated.
	tpl, _ := s.TemplateByID(ctx, templateID)
	if !bytes.Equal(tpl.ImageChecksumSha256, checksum) {
		t.Errorf("template checksum back-prop = %x, want %x", tpl.ImageChecksumSha256, checksum)
	}

	// Second call to the same (template, pool) is idempotent: no duplicate row,
	// the guard still resolves to imageID, checksum/size/format refresh, and the
	// already-set template checksum stays untouched.
	if err := s.EnsureStorageImageProjected(ctx,
		store.CreateStorageImageParams{
			ID: uuid.New(), TemplateID: templateID, PoolID: poolID,
			ChecksumSha256: "cd-checksum", SizeBytes: 5678, Format: "raw",
		},
		store.UpdateTemplateImageChecksumIfNullParams{ID: templateID, ImageChecksumSha256: bytes.Repeat([]byte{0xCD}, 32)},
	); err != nil {
		t.Fatalf("EnsureStorageImageProjected(second): %v", err)
	}

	imgs, _ = s.ListStorageImagesByPool(ctx, store.ListStorageImagesByPoolParams{PoolID: poolID, LimitCount: 50})
	if len(imgs) != 1 || imgs[0].ID != imageID {
		t.Fatalf("after second call images = %+v, want single id %v", imgs, imageID)
	}
	if imgs[0].ChecksumSha256 != "cd-checksum" || imgs[0].SizeBytes != 5678 || imgs[0].Format != "raw" {
		t.Errorf("refreshed image = %+v, want cd-checksum/5678/raw", imgs[0])
	}
	tpl, _ = s.TemplateByID(ctx, templateID)
	if !bytes.Equal(tpl.ImageChecksumSha256, checksum) {
		t.Errorf("template checksum after second call = %x, want unchanged %x", tpl.ImageChecksumSha256, checksum)
	}
}
