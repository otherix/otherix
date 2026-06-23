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
// agent blob produce. The executor seam lives in run.go;
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

// DeleteExecArgs is the per-task input the delete executor receives. VMID keys the
// agent's content-addressed delete (the agent locates the manifest by vm_uuid, not
// the live VM, so deletion works after the VM is gone). OrphanedBlobs is the
// fail-closed-GC set: ONLY digests the CP reference graph proved are no longer
// referenced by any other snapshot. The agent removes exactly these blobs (and the
// snapshot's local manifest); a still-shared blob is never in this set.
type DeleteExecArgs struct {
	VMID               uuid.UUID
	AdvertisedEndpoint string
	SnapshotName       string
	OrphanedBlobs      []string
}

// SnapshotExecutor is the per-task-type agent seam. The production implementation
// is agentSnapshotExecutor over the agentclient package; tests pass an in-package
// fake.
//
// Create is split into Post + Poll so the worker can drive the agent_task_id
// resume seam (mirroring the migrations StartOutgoingMigration + PollTask split):
// Post issues the one-shot agent POST and returns the agent task id (which the
// worker persists BEFORE polling); Poll resumes against that id. On a redelivery
// while the capture is still running, the worker reads the persisted id and calls
// Poll directly, never re-POSTing a second blockdev-backup.
type SnapshotExecutor interface {
	// Post submits the snapshot-create request to the agent and returns the agent
	// task id driving the capture. Idempotency is the worker's responsibility: it
	// calls Post exactly once (when the task carries no agent_task_id yet).
	Post(ctx context.Context, a CreateExecArgs) (agentTaskID uuid.UUID, err error)
	// Poll drives the agent task at agentTaskID to terminal and decodes the
	// agent-reported manifest. Safe to resume across redeliveries.
	Poll(ctx context.Context, endpoint string, agentTaskID uuid.UUID) (CreateExecResult, error)
	Delete(ctx context.Context, a DeleteExecArgs) error
}

// Failure code constants for the task `error` JSONB envelope.
const (
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
