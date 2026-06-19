// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	replication "github.com/otherix/otherix/internal/api/handlers/replication"
	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// TestReconcileReachesKAgainstRealStore drives the REAL durability reconcile pass
// over a live *Store: a digest produced on a single ready node, referenced by a
// snapshot in a Count:2 all-nodes pool, must gain one fresh placement target and
// exactly one pending artifact.replicate task after one pass.
func TestReconcileReachesKAgainstRealStore(t *testing.T) {
	s, cl := startStore(t)
	ctx := context.Background()

	// Three ready nodes (seed rows directly so status is exactly ready).
	seedReadyNode := func(name string) uuid.UUID {
		id := uuid.New()
		n := store.Node{
			ID:                 id,
			Name:               uniqueNodeName(name),
			Architecture:       store.CpuArchAmd64,
			AdvertisedEndpoint: "https://node.example:9443",
			MigrationHost:      "10.0.0.1",
			Status:             store.NodeStatusReady,
			CreatedAt:          time.Now().UTC(),
			UpdatedAt:          time.Now().UTC(),
		}
		if err := cl.PutJSON(ctx, etcd.Key("nodes", id.String()), n); err != nil {
			t.Fatalf("seed node %q: %v", name, err)
		}
		return id
	}
	node1 := seedReadyNode("recon-1")
	_ = seedReadyNode("recon-2")
	_ = seedReadyNode("recon-3")

	poolName := "recon-gold-" + uuid.NewString()[:8]
	if _, err := s.CreateArtifactPool(ctx, store.CreateArtifactPoolParams{
		ID:                uuid.New(),
		Name:              poolName,
		ReplicationFactor: store.ReplicationFactor{Count: 2},
		Membership:        store.ArtifactPoolMembership{AllNodes: true},
	}); err != nil {
		t.Fatalf("CreateArtifactPool: %v", err)
	}

	owner, err := s.CreateUser(ctx, userParams(uniqueEmail("recon")))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	vm := vmRow("recon-src")
	vm.OwnerID = owner.ID
	seedVM(t, cl, vm)

	// Snapshot tagged with the artifact pool (set ArtifactPoolName so K resolves).
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

	digest := "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	if err := s.SnapshotManifestApplied(ctx, snapID, node1, []store.SnapshotDisk{
		{Index: 0, Device: "virtio0", SHA256: digest, SizeBytes: 4096, Format: "qcow2"},
	}, store.VmStateAtSnapshotStopped); err != nil {
		t.Fatalf("SnapshotManifestApplied: %v", err)
	}
	// node1 observed holding the blob, so there is a live holder to pull from.
	if err := s.UpsertNodeBlobInventory(ctx, node1, []store.NodeBlob{
		{Digest: digest, SizeBytes: 4096},
	}); err != nil {
		t.Fatalf("UpsertNodeBlobInventory: %v", err)
	}

	// Pre-state: exactly one placement member (node1, from SnapshotManifestApplied).
	if got, err := s.BlobPlacements(ctx, digest); err != nil || len(got) != 1 {
		t.Fatalf("pre-reconcile BlobPlacements = %v, %v; want one member", got, err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := replication.ReconcileFunc(s, log)(ctx); err != nil {
		t.Fatalf("ReconcileFunc: %v", err)
	}

	members, err := s.BlobPlacements(ctx, digest)
	if err != nil {
		t.Fatalf("post-reconcile BlobPlacements: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("post-reconcile placement members = %d, want 2 (one target added to reach K)", len(members))
	}

	pending := string(store.TaskStatusPending)
	typ := "artifact.replicate"
	tasks, err := s.ListTasksAny(ctx, store.ListTasksAnyParams{
		StatusFilter: &pending,
		TypeFilter:   &typ,
		LimitCount:   100,
	})
	if err != nil {
		t.Fatalf("ListTasksAny: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("pending artifact.replicate tasks = %d, want exactly 1", len(tasks))
	}

	// The target chosen on the first pass (the new placement member that is not node1).
	target := members[0]
	if target == node1 {
		target = members[1]
	}

	countPending := func() int {
		t.Helper()
		got, err := s.ListTasksAny(ctx, store.ListTasksAnyParams{StatusFilter: &pending, TypeFilter: &typ, LimitCount: 100})
		if err != nil {
			t.Fatalf("ListTasksAny: %v", err)
		}
		return len(got)
	}

	// Second pass while the in-flight marker is set: dedup must enqueue NOTHING new.
	if err := replication.ReconcileFunc(s, log)(ctx); err != nil {
		t.Fatalf("ReconcileFunc (2nd pass): %v", err)
	}
	if n := countPending(); n != 1 {
		t.Fatalf("after 2nd pass pending artifact.replicate tasks = %d, want still 1 (dedup)", n)
	}

	// Clear the marker; a later pass re-enqueues (target still not holding the blob).
	if err := s.EndReplicate(ctx, digest, target); err != nil {
		t.Fatalf("EndReplicate: %v", err)
	}
	if err := replication.ReconcileFunc(s, log)(ctx); err != nil {
		t.Fatalf("ReconcileFunc (3rd pass): %v", err)
	}
	if n := countPending(); n != 2 {
		t.Fatalf("after marker clear pending artifact.replicate tasks = %d, want 2 (re-enqueue)", n)
	}
}
