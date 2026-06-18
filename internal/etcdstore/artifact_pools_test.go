// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

func TestArtifactPoolCRUD(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	ap, err := s.CreateArtifactPool(ctx, store.CreateArtifactPoolParams{
		ID:                uuid.New(),
		Name:              "gold",
		ReplicationFactor: store.ReplicationFactor{Count: 3},
		Membership:        store.ArtifactPoolMembership{AllNodes: true},
	})
	if err != nil {
		t.Fatalf("CreateArtifactPool: %v", err)
	}

	got, err := s.ArtifactPoolByID(ctx, ap.ID)
	if err != nil || got.Name != "gold" {
		t.Fatalf("ArtifactPoolByID = %+v, %v", got, err)
	}
	byName, err := s.ArtifactPoolByName(ctx, "GOLD")
	if err != nil || byName.ID != ap.ID {
		t.Fatalf("ArtifactPoolByName = %+v, %v", byName, err)
	}

	if _, err := s.CreateArtifactPool(ctx, store.CreateArtifactPoolParams{
		ID: uuid.New(), Name: "gold", ReplicationFactor: store.ReplicationFactor{Count: 1},
	}); !errors.Is(err, store.ErrArtifactPoolNameExists) {
		t.Fatalf("duplicate create err = %v, want ErrArtifactPoolNameExists", err)
	}

	pools, err := s.ListArtifactPools(ctx, store.ListArtifactPoolsParams{LimitCount: 50})
	if err != nil || len(pools) != 1 {
		t.Fatalf("ListArtifactPools = %d pools, %v", len(pools), err)
	}

	if err := s.DeleteArtifactPool(ctx, ap.ID); err != nil {
		t.Fatalf("DeleteArtifactPool: %v", err)
	}
	if err := s.DeleteArtifactPool(ctx, ap.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete err = %v, want ErrNotFound", err)
	}
	if _, err := s.CreateArtifactPool(ctx, store.CreateArtifactPoolParams{
		ID: uuid.New(), Name: "gold", ReplicationFactor: store.ReplicationFactor{All: true},
	}); err != nil {
		t.Fatalf("recreate after delete: %v", err)
	}
}

func TestArtifactPoolDeleteFailClosed(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	user, err := s.CreateUser(ctx, userParams(uniqueEmail("apdel")))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	owner := user.ID

	ap, err := s.CreateArtifactPool(ctx, store.CreateArtifactPoolParams{
		ID: uuid.New(), Name: "gold", ReplicationFactor: store.ReplicationFactor{Count: 1},
	})
	if err != nil {
		t.Fatalf("CreateArtifactPool: %v", err)
	}

	vmID := uuid.New()
	name := "gold"
	if _, err := s.CreateSnapshot(ctx, store.CreateSnapshotParams{
		ID: uuid.New(), VmID: vmID, OwnerID: owner, Name: "s1",
		VMStateAtSnapshot:  store.VmStateAtSnapshotStopped,
		SourceArchitecture: store.CpuArchAmd64,
		ArtifactPoolName:   &name,
		Task: store.CreateTaskParams{
			ID: uuid.New(), Type: "vm.snapshot.create", Status: store.TaskStatusPending,
			ResourceType: "snapshot", MaxAttempts: 25, CreatedBy: &owner,
		},
	}, stubSnapArgs{}); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	var inUse *store.ResourceInUseError
	err = s.DeleteArtifactPool(ctx, ap.ID)
	if !errors.As(err, &inUse) || inUse.Resources["snapshots"] != 1 {
		t.Fatalf("delete err = %v, want ResourceInUseError{snapshots:1}", err)
	}
}

func TestArtifactPoolCrossNamespaceName(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	n, err := s.CreateNode(ctx, nodeParams(uniqueNodeName("apns")))
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	node := n.ID

	if _, err := s.CreateStoragePool(ctx, store.CreateStoragePoolParams{
		ID: uuid.New(), NodeID: node, Name: "shared", Type: "local_dir",
		Path: "/var/lib/otherix/pools/shared", Config: []byte("{}"),
	}); err != nil {
		t.Fatalf("CreateStoragePool: %v", err)
	}
	if _, err := s.CreateArtifactPool(ctx, store.CreateArtifactPoolParams{
		ID: uuid.New(), Name: "shared", ReplicationFactor: store.ReplicationFactor{Count: 1},
	}); !errors.Is(err, store.ErrPoolNameConflict) {
		t.Fatalf("artifact create over disk name err = %v, want ErrPoolNameConflict", err)
	}

	if _, err := s.CreateArtifactPool(ctx, store.CreateArtifactPoolParams{
		ID: uuid.New(), Name: "gold", ReplicationFactor: store.ReplicationFactor{Count: 1},
	}); err != nil {
		t.Fatalf("CreateArtifactPool: %v", err)
	}
	if _, err := s.CreateStoragePool(ctx, store.CreateStoragePoolParams{
		ID: uuid.New(), NodeID: node, Name: "gold", Type: "local_dir",
		Path: "/var/lib/otherix/pools/gold", Config: []byte("{}"),
	}); !errors.Is(err, store.ErrPoolNameConflict) {
		t.Fatalf("disk create over artifact name err = %v, want ErrPoolNameConflict", err)
	}
}
