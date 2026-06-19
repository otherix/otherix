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

func TestPlacementReadAndMutate(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	n1, n2 := uuid.New(), uuid.New()

	if digests, err := s.AllPlacementDigests(ctx); err != nil || len(digests) != 0 {
		t.Fatalf("AllPlacementDigests empty = %v, %v; want [], nil", digests, err)
	}
	if err := s.AddBlobPlacement(ctx, digest, n1); err != nil {
		t.Fatalf("AddBlobPlacement n1: %v", err)
	}
	if err := s.AddBlobPlacement(ctx, digest, n2); err != nil {
		t.Fatalf("AddBlobPlacement n2: %v", err)
	}
	got, err := s.BlobPlacements(ctx, digest)
	if err != nil {
		t.Fatalf("BlobPlacements: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("BlobPlacements len = %d, want 2", len(got))
	}
	digests, err := s.AllPlacementDigests(ctx)
	if err != nil || len(digests) != 1 || digests[0] != digest {
		t.Fatalf("AllPlacementDigests = %v, %v; want [%s]", digests, err, digest)
	}
	removed, err := s.RemoveBlobPlacement(ctx, digest, n1)
	if err != nil || !removed {
		t.Fatalf("RemoveBlobPlacement = %v, %v; want true, nil", removed, err)
	}
	got, _ = s.BlobPlacements(ctx, digest)
	if len(got) != 1 || got[0] != n2 {
		t.Fatalf("after remove BlobPlacements = %v, want [%s]", got, n2)
	}
	removed, err = s.RemoveBlobPlacement(ctx, digest, n1)
	if err != nil || removed {
		t.Fatalf("RemoveBlobPlacement absent = %v, %v; want false, nil", removed, err)
	}
}

// TestSnapshotsReferencingBlob proves the reverse of the snapshot reference-graph
// write: SnapshotManifestApplied records one blobRef entry per disk under each
// digest, and SnapshotsReferencingBlob walks that prefix back to the snapshot id.
func TestSnapshotsReferencingBlob(t *testing.T) {
	s, cl := startStore(t)
	ctx := context.Background()

	owner, err := s.CreateUser(ctx, userParams(uniqueEmail("snaprefblob")))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	vm := vmRow("snap-refblob-src")
	vm.OwnerID = owner.ID
	seedVM(t, cl, vm)

	nodeID := uuid.New()
	snap := seedSnapshot(t, s, ctx, vm.ID, owner.ID, "daily")
	diskDigest := "cc"
	if err := s.SnapshotManifestApplied(ctx, snap.ID, nodeID, []store.SnapshotDisk{
		{Index: 0, Device: "virtio0", SHA256: diskDigest, SizeBytes: 10, Format: "qcow2"},
	}, store.VmStateAtSnapshotStopped); err != nil {
		t.Fatalf("SnapshotManifestApplied: %v", err)
	}

	refs, err := s.SnapshotsReferencingBlob(ctx, diskDigest)
	if err != nil {
		t.Fatalf("SnapshotsReferencingBlob: %v", err)
	}
	if len(refs) != 1 || refs[0] != snap.ID {
		t.Fatalf("SnapshotsReferencingBlob = %v, want [%s]", refs, snap.ID)
	}

	if empty, err := s.SnapshotsReferencingBlob(ctx, "no-such-digest"); err != nil || len(empty) != 0 {
		t.Fatalf("SnapshotsReferencingBlob(absent) = %v, %v; want [], nil", empty, err)
	}
}

// TestAllNodes proves AllNodes returns only non-deleted node rows with Status
// preserved, so a soft-deleted node drops out of the live-node set.
func TestAllNodes(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	keep, err := s.CreateNode(ctx, nodeParams(uniqueNodeName("allnodes-keep")))
	if err != nil {
		t.Fatalf("CreateNode keep: %v", err)
	}
	drop, err := s.CreateNode(ctx, nodeParams(uniqueNodeName("allnodes-drop")))
	if err != nil {
		t.Fatalf("CreateNode drop: %v", err)
	}
	if _, err := s.DeleteNode(ctx, drop.ID, false, uuid.New()); err != nil {
		t.Fatalf("DeleteNode drop: %v", err)
	}

	nodes, err := s.AllNodes(ctx)
	if err != nil {
		t.Fatalf("AllNodes: %v", err)
	}
	for _, n := range nodes {
		if n.ID == drop.ID {
			t.Fatalf("AllNodes returned soft-deleted node %s", drop.ID)
		}
	}
	var found *store.Node
	for i := range nodes {
		if nodes[i].ID == keep.ID {
			found = &nodes[i]
		}
	}
	if found == nil {
		t.Fatalf("AllNodes missing live node %s", keep.ID)
	}
	if found.Status != store.NodeStatusPending {
		t.Fatalf("AllNodes node Status = %q, want %q (preserved)", found.Status, store.NodeStatusPending)
	}
}
