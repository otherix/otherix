// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package snapshots

import (
	"context"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

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

// CreateExecArgs is the per-task input the create executor receives. The worker
// resolves snapshot -> VM -> node before dispatching, so the executor sees a
// self-contained, dependency-free struct. VMName + AdvertisedEndpoint name-key the
// agent call; SnapshotName + Description are the snapshot the agent produces.
type CreateExecArgs struct {
	VMName             string
	AdvertisedEndpoint string
	SnapshotName       string
	Description        string
}

// CreateExecResult is the create executor's output: the content-addressed manifest
// the agent actually captured. The agent is authoritative for both the per-disk
// blob digests and the VM state at capture, so the worker projects these verbatim
// into the snapshot row (it never re-derives vm_state_at_snapshot from CP-side
// runtime, which could have drifted since enqueue).
type CreateExecResult struct {
	Disks             []store.SnapshotDisk
	VMStateAtSnapshot store.VMStateAtSnapshot
}

// DeleteExecArgs is the per-task input the delete executor receives. OrphanedBlobs
// is the fail-closed-GC set: ONLY digests the CP reference graph proved are no
// longer referenced by any other snapshot. The agent removes exactly these blobs
// (and the snapshot's local manifest); a still-shared blob is never in this set.
type DeleteExecArgs struct {
	VMName             string
	AdvertisedEndpoint string
	SnapshotName       string
	OrphanedBlobs      []string
}

// SnapshotExecutor is the per-task-type agent seam. The production implementation
// is agentSnapshotExecutor over the agentclient package; tests pass an in-package
// fake.
type SnapshotExecutor interface {
	Create(ctx context.Context, a CreateExecArgs) (CreateExecResult, error)
	Delete(ctx context.Context, a DeleteExecArgs) error
}

// Failure code constants for the task `error` JSONB envelope.
const (
	errCodeVMNotFound      = "vm_not_found"
	errCodeNodeUnreachable = "node_unreachable"
	errCodeSnapshotFailed  = "snapshot_failed"
)

// taskErrorJSON is the inner-error envelope written into tasks.error. Mirrors the
// public Error.error shape (code + message); details are not populated by task
// workers.
type taskErrorJSON struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
