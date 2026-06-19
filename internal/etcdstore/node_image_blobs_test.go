// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

func TestImageBlobHolders(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	n1, n2 := uuid.New(), uuid.New()
	digest := strings.Repeat("a", 64)

	if err := s.UpsertNodeImageBlobInventory(ctx, n1, []store.NodeBlob{{Digest: digest, SizeBytes: 10}}); err != nil {
		t.Fatalf("upsert n1: %v", err)
	}
	if err := s.UpsertNodeImageBlobInventory(ctx, n2, []store.NodeBlob{{Digest: digest, SizeBytes: 10}}); err != nil {
		t.Fatalf("upsert n2: %v", err)
	}
	holders, err := s.ImageBlobHolders(ctx, digest)
	if err != nil {
		t.Fatalf("holders: %v", err)
	}
	if len(holders) != 2 {
		t.Errorf("ImageBlobHolders(%s) = %d holders, want 2", digest, len(holders))
	}
}

func TestImageBlobInventoryEmptyDeletes(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	n := uuid.New()
	digest := strings.Repeat("b", 64)
	if err := s.UpsertNodeImageBlobInventory(ctx, n, []store.NodeBlob{{Digest: digest, SizeBytes: 1}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.UpsertNodeImageBlobInventory(ctx, n, nil); err != nil {
		t.Fatalf("empty upsert: %v", err)
	}
	holders, err := s.ImageBlobHolders(ctx, digest)
	if err != nil {
		t.Fatalf("holders: %v", err)
	}
	if len(holders) != 0 {
		t.Errorf("after empty upsert, holders = %d, want 0", len(holders))
	}
}

func TestImageBlobInventoryIsolatedFromNodeBlobs(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	n := uuid.New()
	digest := strings.Repeat("c", 64)
	if err := s.UpsertNodeImageBlobInventory(ctx, n, []store.NodeBlob{{Digest: digest, SizeBytes: 5}}); err != nil {
		t.Fatalf("upsert image: %v", err)
	}
	// The artifact-tier holder discovery must NOT see an image-tier blob.
	holders, err := s.BlobHolders(ctx, digest)
	if err != nil {
		t.Fatalf("blob holders: %v", err)
	}
	if len(holders) != 0 {
		t.Errorf("BlobHolders saw an image-tier blob: %d holders, want 0 (families must be isolated)", len(holders))
	}
	// And the reconcile-feeding digest scan must NOT include it.
	digests, err := s.AllNodeBlobDigests(ctx)
	if err != nil {
		t.Fatalf("all node blob digests: %v", err)
	}
	for _, d := range digests {
		if d == digest {
			t.Errorf("AllNodeBlobDigests included an image-tier digest - isolation broken")
		}
	}
}
