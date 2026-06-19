// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

func TestNodeBlobInventoryUpsertAndRead(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	node := uuid.New()

	blobs := []store.NodeBlob{{Digest: "aa", SizeBytes: 10}, {Digest: "bb", SizeBytes: 20}}
	if err := s.UpsertNodeBlobInventory(ctx, node, blobs); err != nil {
		t.Fatalf("UpsertNodeBlobInventory: %v", err)
	}
	got, err := s.NodeBlobInventory(ctx, node)
	if err != nil {
		t.Fatalf("NodeBlobInventory: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("inventory len = %d, want 2", len(got))
	}
	if err := s.UpsertNodeBlobInventory(ctx, node, nil); err != nil {
		t.Fatalf("clear upsert: %v", err)
	}
	got, err = s.NodeBlobInventory(ctx, node)
	if err != nil {
		t.Fatalf("NodeBlobInventory after clear: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("inventory after clear = %d, want 0", len(got))
	}
}

func TestBlobHolders(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	n1, n2, n3 := uuid.New(), uuid.New(), uuid.New()
	d := "00000000000000000000000000000000000000000000000000000000000000ab"

	if err := s.UpsertNodeBlobInventory(ctx, n1, []store.NodeBlob{{Digest: d, SizeBytes: 1}}); err != nil {
		t.Fatalf("n1 upsert: %v", err)
	}
	if err := s.UpsertNodeBlobInventory(ctx, n2, []store.NodeBlob{{Digest: d, SizeBytes: 1}}); err != nil {
		t.Fatalf("n2 upsert: %v", err)
	}
	if err := s.UpsertNodeBlobInventory(ctx, n3, []store.NodeBlob{{Digest: "ff", SizeBytes: 1}}); err != nil {
		t.Fatalf("n3 upsert: %v", err)
	}

	holders, err := s.BlobHolders(ctx, d)
	if err != nil {
		t.Fatalf("BlobHolders: %v", err)
	}
	if len(holders) != 2 {
		t.Fatalf("holders = %v, want 2 (n1,n2)", holders)
	}
	set := map[uuid.UUID]bool{}
	for _, h := range holders {
		set[h] = true
	}
	if !set[n1] || !set[n2] {
		t.Errorf("holders = %v, want {n1,n2}", holders)
	}
}

func TestBlobSize(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	node := uuid.New()
	d := "00000000000000000000000000000000000000000000000000000000000000ab"

	if err := s.UpsertNodeBlobInventory(ctx, node, []store.NodeBlob{{Digest: d, SizeBytes: 4096}}); err != nil {
		t.Fatalf("UpsertNodeBlobInventory: %v", err)
	}

	size, ok := s.BlobSize(ctx, d)
	if !ok || size != 4096 {
		t.Errorf("BlobSize(%s) = (%d, %v), want (4096, true)", d, size, ok)
	}

	absent := "00000000000000000000000000000000000000000000000000000000000000cd"
	if size, ok := s.BlobSize(ctx, absent); ok || size != 0 {
		t.Errorf("BlobSize(absent) = (%d, %v), want (0, false)", size, ok)
	}
}

func TestBlobHoldersNoneWhenAbsent(t *testing.T) {
	s, _ := startStore(t)
	holders, err := s.BlobHolders(context.Background(), "00000000000000000000000000000000000000000000000000000000000000cd")
	if err != nil {
		t.Fatalf("BlobHolders: %v", err)
	}
	if len(holders) != 0 {
		t.Errorf("holders for absent digest = %v, want empty", holders)
	}
}
