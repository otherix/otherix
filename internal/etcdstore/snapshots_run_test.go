// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	snapshotshandlers "github.com/otherix/otherix/internal/api/handlers/snapshots"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// The etcd store satisfies the snapshot worker handlers' storage contract.
var _ snapshotshandlers.WorkerStore = (*etcdstore.Store)(nil)

// stubCreateExec is a SnapshotExecutor double returning a fixed two-disk manifest;
// the run handler drives it against the REAL store so the seam test covers the
// manifest projection + task finalize, not a direct method call.
type stubCreateExec struct {
	res snapshotshandlers.CreateExecResult
	err error
}

func (s stubCreateExec) Create(context.Context, snapshotshandlers.CreateExecArgs) (snapshotshandlers.CreateExecResult, error) {
	return s.res, s.err
}

func (s stubCreateExec) Delete(context.Context, snapshotshandlers.DeleteExecArgs) error { return nil }

// TestSnapshotCreateRunHandler drives the real CreateHandler against the embedded
// store: it seeds a snapshot + a running VM runtime + node, runs the worker, and
// asserts the agent-reported manifest projected (disks + vm_state) and the task
// reached success - the REAL CP->worker->store projection sequence.
func TestSnapshotCreateRunHandler(t *testing.T) {
	s, cl := startStore(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	owner, err := s.CreateUser(ctx, userParams(uniqueEmail("snaprun")))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	node := seedNodeWithEndpoint(t, s, "snap-run-node", "https://snap-run-node:9443")
	vm := vmRow("snap-run-src")
	vm.OwnerID = owner.ID
	seedVM(t, cl, vm)
	placeVM(t, cl, vm.ID, node.ID)

	snap := seedSnapshot(t, s, ctx, vm.ID, owner.ID, "daily")
	// The backing task was enqueued by CreateSnapshot under snap's task; reuse a
	// fresh enqueue so the handler claims a known task id.
	task := taskParams(store.TaskStatusPending, &snap.ID)
	if _, err := s.EnqueueTask(ctx, task, stubSnapArgs{TaskID: task.ID, SnapshotID: snap.ID}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	raw, _ := json.Marshal(snapshotshandlers.SnapshotCreateArgs{TaskID: task.ID, SnapshotID: snap.ID})
	exec := stubCreateExec{res: snapshotshandlers.CreateExecResult{
		Disks: []store.SnapshotDisk{
			{Index: 0, Device: "virtio0", SHA256: "aa", SizeBytes: 10, Format: "qcow2"},
			{Index: 1, Device: "virtio1", SHA256: "bb", SizeBytes: 20, Format: "qcow2"},
		},
		VMStateAtSnapshot: store.VmStateAtSnapshotRunning,
	}}
	if err := snapshotshandlers.CreateHandler(s, exec, log)(ctx, raw); err != nil {
		t.Fatalf("create handler: %v", err)
	}

	got, err := s.SnapshotByID(ctx, snap.ID)
	if err != nil {
		t.Fatalf("SnapshotByID: %v", err)
	}
	if got.Status != store.SnapshotStatusReady {
		t.Errorf("status = %q, want ready", got.Status)
	}
	if len(got.Disks) != 2 || got.VMStateAtSnapshot != store.VmStateAtSnapshotRunning {
		t.Errorf("projected manifest = %+v / %q, want 2 disks + running", got.Disks, got.VMStateAtSnapshot)
	}
	tk, _ := s.TaskByID(ctx, task.ID)
	if tk.Status != store.TaskStatusSuccess {
		t.Errorf("task = %v, want success", tk.Status)
	}
}
