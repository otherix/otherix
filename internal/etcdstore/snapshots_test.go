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

// stubSnapArgs is a minimal queue.JobArgs the snapshot enqueue path can marshal
// onto the backing job. The concrete SnapshotCreateArgs lives in the handlers
// package (built in a later task); CreateSnapshot takes the queue.JobArgs
// interface, so any Kind()-implementer works here.
type stubSnapArgs struct {
	TaskID     uuid.UUID
	SnapshotID uuid.UUID
}

func (stubSnapArgs) Kind() string { return "vm.snapshot.create" }

func TestCreateSnapshot_WritesRowOwnerIndexAndTask(t *testing.T) {
	s, cl := startStore(t)
	ctx := context.Background()

	owner, err := s.CreateUser(ctx, userParams(uniqueEmail("snapowner")))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	vm := vmRow("snap-src")
	vm.OwnerID = owner.ID
	seedVM(t, cl, vm)

	sid := uuid.New()
	taskID := uuid.New()
	got, err := s.CreateSnapshot(ctx, store.CreateSnapshotParams{
		ID: sid, VmID: vm.ID, OwnerID: owner.ID, Name: "daily",
		VMStateAtSnapshot: store.VmStateAtSnapshotStopped,
		Task: store.CreateTaskParams{
			ID: taskID, Type: "vm.snapshot.create", Status: store.TaskStatusPending,
			ResourceType: "snapshot", ResourceID: &sid, MaxAttempts: 25, CreatedBy: &owner.ID,
		},
	}, stubSnapArgs{TaskID: taskID, SnapshotID: sid})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if got.ID != sid || got.Status != store.SnapshotStatusCreating {
		t.Errorf("CreateSnapshot = {id:%v status:%q}, want {id:%v status:creating}", got.ID, got.Status, sid)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("CreateSnapshot timestamps not stamped: created=%v updated=%v", got.CreatedAt, got.UpdatedAt)
	}
	if len(got.Disks) != 0 {
		t.Errorf("CreateSnapshot Disks = %v, want empty (filled by worker on success)", got.Disks)
	}

	// Primary row reads back at status=creating.
	read, err := s.SnapshotByID(ctx, sid)
	if err != nil || read.Status != store.SnapshotStatusCreating {
		t.Fatalf("SnapshotByID = (%+v, %v); want status creating", read, err)
	}

	// The owner-index entry drives CountUserResources.
	cnt, err := s.CountUserResources(ctx, owner.ID)
	if err != nil {
		t.Fatalf("CountUserResources: %v", err)
	}
	if cnt.Snapshots != 1 {
		t.Errorf("CountUserResources.Snapshots = %d, want 1", cnt.Snapshots)
	}

	// The backing task was enqueued atomically and is readable.
	tsk, err := s.TaskByID(ctx, taskID)
	if err != nil {
		t.Fatalf("TaskByID(backing task): %v", err)
	}
	if tsk.Type != "vm.snapshot.create" || tsk.Status != store.TaskStatusPending {
		t.Errorf("backing task = {type:%q status:%q}, want {vm.snapshot.create pending}", tsk.Type, tsk.Status)
	}

	// A duplicate name within the same VM (different case) is rejected by the guard.
	if _, err := s.CreateSnapshot(ctx, store.CreateSnapshotParams{
		ID: uuid.New(), VmID: vm.ID, OwnerID: owner.ID, Name: "Daily",
		VMStateAtSnapshot: store.VmStateAtSnapshotStopped,
		Task: store.CreateTaskParams{
			ID: uuid.New(), Type: "vm.snapshot.create", Status: store.TaskStatusPending,
		},
	}, stubSnapArgs{}); !errors.Is(err, store.ErrSnapshotNameExists) {
		t.Errorf("duplicate name err = %v, want store.ErrSnapshotNameExists", err)
	}
}
