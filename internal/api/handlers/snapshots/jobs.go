// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package snapshots

import "github.com/google/uuid"

// SnapshotCreateArgs is the queue.JobArgs payload for the vm.snapshot.create
// task: the backing task id and the snapshot id the worker resolves to drive the
// agent blob produce. Task 6 extends this file with the executor seam + run.go;
// the struct lives here so the create handler can enqueue the job atomically
// with the snapshot row.
type SnapshotCreateArgs struct {
	TaskID     uuid.UUID
	SnapshotID uuid.UUID
}

// Kind is the queue dispatch key for snapshot creation.
func (SnapshotCreateArgs) Kind() string { return "vm.snapshot.create" }

// SnapshotDeleteArgs is the queue.JobArgs payload for the vm.snapshot.delete
// task: the backing task id and the snapshot id whose blobs the worker
// best-effort GCs (gated on the CP reference graph). Enqueued by the delete
// handler after the row is soft-deleted (fail-closed) CP-side.
type SnapshotDeleteArgs struct {
	TaskID     uuid.UUID
	SnapshotID uuid.UUID
}

// Kind is the queue dispatch key for snapshot deletion.
func (SnapshotDeleteArgs) Kind() string { return "vm.snapshot.delete" }
