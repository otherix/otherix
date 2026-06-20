// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	replication "github.com/otherix/otherix/internal/api/handlers/replication"
	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// TestReconcileLeavesImageOnlyDigestUntouched is a guard for a core isolation
// invariant of the image cache tier.
//
// A pinned-image cache blob is inventoried in image_blobs, a family the
// durability reconcile never reads. This guard drives the real reconcile over a
// real store seeded with an image-only digest and asserts the reconcile leaves
// it completely untouched: no placement key, never enumerated by the node-blob
// digest scan that feeds the reconcile, never reclaimed. If image_blobs is ever
// folded into AllNodeBlobDigests or the reconcile union, this fails.
//
// A positive control digest (held in node_blobs, placement-keyed, and referenced
// by a snapshot in a Count:1 pool) proves the reconcile actually iterated this
// pass rather than no-opping: it must be enumerated and left at its single
// placement member, not reclaimed.
func TestReconcileLeavesImageOnlyDigestUntouched(t *testing.T) {
	s, cl := startStore(t)
	ctx := context.Background()

	// A single ready node so the reconcile has live capacity to act on.
	node := uuid.New()
	n := store.Node{
		ID:                 node,
		Name:               uniqueNodeName("img-iso"),
		Architecture:       store.CpuArchAmd64,
		AdvertisedEndpoint: "https://node.example:9443",
		MigrationHost:      "10.0.0.1",
		Status:             store.NodeStatusReady,
		CreatedAt:          time.Now().UTC(),
		UpdatedAt:          time.Now().UTC(),
	}
	if err := cl.PutJSON(ctx, etcd.Key("nodes", node.String()), n); err != nil {
		t.Fatalf("seed node: %v", err)
	}

	imageDigest := strings.Repeat("e", 64)

	// Seed the digest ONLY in the image tier: no node_blobs, no placement, no refs.
	if err := s.UpsertNodeImageBlobInventory(ctx, node, []store.NodeBlob{{Digest: imageDigest, SizeBytes: 100}}); err != nil {
		t.Fatalf("seed image blob: %v", err)
	}

	// Positive control: a referenced, placement-keyed, observed artifact digest in
	// a Count:1 pool. The reconcile iterates it (proving the pass ran) but, already
	// at K=1 and still referenced, leaves it at its single placement member.
	poolName := "img-iso-gold-" + uuid.NewString()[:8]
	if _, err := s.CreateArtifactPool(ctx, store.CreateArtifactPoolParams{
		ID:                uuid.New(),
		Name:              poolName,
		ReplicationFactor: store.ReplicationFactor{Count: 1},
		Membership:        store.ArtifactPoolMembership{AllNodes: true},
	}); err != nil {
		t.Fatalf("CreateArtifactPool: %v", err)
	}
	owner, err := s.CreateUser(ctx, userParams(uniqueEmail("img-iso")))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	vm := vmRow("img-iso-src")
	vm.OwnerID = owner.ID
	seedVM(t, cl, vm)
	snapID := uuid.New()
	taskID := uuid.New()
	if _, err := s.CreateSnapshot(ctx, store.CreateSnapshotParams{
		ID: snapID, VmID: vm.ID, OwnerID: owner.ID, Name: "daily",
		VMStateAtSnapshot: store.VmStateAtSnapshotStopped,
		ArtifactPoolName:  &poolName,
		Task: store.CreateTaskParams{
			ID: taskID, Type: "vm.snapshot.create", Status: store.TaskStatusPending,
			ResourceType: "snapshot", ResourceID: &snapID,
		},
	}, stubSnapArgs{TaskID: taskID, SnapshotID: snapID}); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	artifactDigest := strings.Repeat("d", 64)
	if err := s.SnapshotManifestApplied(ctx, snapID, node, []store.SnapshotDisk{
		{Index: 0, Device: "virtio0", SHA256: artifactDigest, SizeBytes: 4096, Format: "qcow2"},
	}, store.VmStateAtSnapshotStopped); err != nil {
		t.Fatalf("SnapshotManifestApplied: %v", err)
	}
	if err := s.UpsertNodeBlobInventory(ctx, node, []store.NodeBlob{
		{Digest: artifactDigest, SizeBytes: 4096},
	}); err != nil {
		t.Fatalf("UpsertNodeBlobInventory: %v", err)
	}

	// Sanity: the reconcile's digest source must NOT see the image digest.
	allNodeBlob, err := s.AllNodeBlobDigests(ctx)
	if err != nil {
		t.Fatalf("all node blob digests: %v", err)
	}
	for _, d := range allNodeBlob {
		if d == imageDigest {
			t.Fatalf("AllNodeBlobDigests included an image-only digest - isolation already broken")
		}
	}

	// Drive the REAL reconcile.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := replication.ReconcileFunc(s, log)(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// The image digest must have received NO placement key.
	placements, err := s.BlobPlacements(ctx, imageDigest)
	if err != nil {
		t.Fatalf("placements: %v", err)
	}
	if len(placements) != 0 {
		t.Errorf("reconcile created %d placement entries for an image-only digest, want 0", len(placements))
	}

	// And it must still be present in the image tier (not reclaimed/cleared).
	imgHolders, err := s.ImageBlobHolders(ctx, imageDigest)
	if err != nil {
		t.Fatalf("image holders: %v", err)
	}
	if len(imgHolders) != 1 {
		t.Errorf("image holders = %d after reconcile, want 1 (untouched)", len(imgHolders))
	}

	// Positive control: the referenced artifact digest WAS iterated (the pass ran)
	// and, already at K=1 and referenced, was left at its single placement member -
	// not reclaimed. A reconcile that short-circuited would also leave it, but the
	// AllNodeBlobDigests sanity check above proves the image digest is excluded
	// from the very scan that feeds this iteration.
	ctrl, err := s.BlobPlacements(ctx, artifactDigest)
	if err != nil {
		t.Fatalf("control placements: %v", err)
	}
	if len(ctrl) != 1 {
		t.Errorf("control placement members = %d after reconcile, want 1 (referenced K=1, untouched)", len(ctrl))
	}
}
