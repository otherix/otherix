// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package snapshots

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// Run-form snapshot workers for the etcd job runtime. They reuse the executor seam
// + Args payloads in jobs.go and drive the orchestration against the WorkerStore
// interface (task mutators + entity reads + the manifest projection / blob
// dereference).
//
// create resolves snapshot -> VM -> node (current node from VM runtime), POSTs the
// snapshot to the agent, polls to terminal, and projects the AGENT-reported
// manifest (disks + vm_state_at_snapshot - the agent is authoritative for what was
// actually captured) before finalizing the task. delete dereferences the
// soft-deleted snapshot's blobs in the CP reference graph (fail-closed: only
// truly-orphaned digests come back) and hands ONLY those to the agent to GC.

// WorkerStore is the storage surface the snapshot worker handlers depend on.
// *etcdstore.Store satisfies it (asserted in the etcdstore integration tests).
type WorkerStore interface {
	UpdateTaskRunning(ctx context.Context, id uuid.UUID) (alreadyTerminal bool, err error)
	UpdateTaskFinalized(ctx context.Context, arg store.UpdateTaskFinalizedParams) error
	SnapshotByID(ctx context.Context, id uuid.UUID) (store.Snapshot, error)
	// SnapshotByIDIncludingDeleted reads a snapshot row even when soft-deleted - the
	// delete worker runs AFTER the row is soft-deleted CP-side, so it needs the
	// manifest digests off the deleted row to drive the blob dereference.
	SnapshotByIDIncludingDeleted(ctx context.Context, id uuid.UUID) (store.Snapshot, error)
	VMByID(ctx context.Context, id uuid.UUID) (store.VM, error)
	VMRuntimeByID(ctx context.Context, vmID uuid.UUID) (store.VMRuntime, error)
	NodeByID(ctx context.Context, id uuid.UUID) (store.Node, error)
	// SnapshotManifestApplied projects the agent-reported manifest (disks +
	// vm_state_at_snapshot) and writes the blob reference-graph entries in one Txn.
	SnapshotManifestApplied(ctx context.Context, id uuid.UUID, disks []store.SnapshotDisk, vmState store.VMStateAtSnapshot) error
	// DereferenceSnapshotBlobs removes this snapshot's reference-graph entries for
	// the given digests and returns ONLY the digests left with zero remaining refs
	// (the orphaned set safe to GC). Fail-closed: a digest still referenced by
	// another snapshot is never returned.
	DereferenceSnapshotBlobs(ctx context.Context, snapshotID uuid.UUID, digests []string) (orphaned []string, err error)
}

// CreateHandler returns the dispatcher handler for vm.snapshot.create jobs.
func CreateHandler(st WorkerStore, exec SnapshotExecutor, log *slog.Logger) func(context.Context, []byte) error {
	return func(ctx context.Context, raw []byte) error {
		var args SnapshotCreateArgs
		if err := json.Unmarshal(raw, &args); err != nil {
			return fmt.Errorf("unmarshal vm.snapshot.create args: %v", err)
		}
		return runSnapshotCreate(ctx, st, exec, log, args)
	}
}

// DeleteHandler returns the dispatcher handler for vm.snapshot.delete jobs.
func DeleteHandler(st WorkerStore, exec SnapshotExecutor, log *slog.Logger) func(context.Context, []byte) error {
	return func(ctx context.Context, raw []byte) error {
		var args SnapshotDeleteArgs
		if err := json.Unmarshal(raw, &args); err != nil {
			return fmt.Errorf("unmarshal vm.snapshot.delete args: %v", err)
		}
		return runSnapshotDelete(ctx, st, exec, log, args)
	}
}

func runSnapshotCreate(ctx context.Context, st WorkerStore, exec SnapshotExecutor, log *slog.Logger, args SnapshotCreateArgs) error {
	taskID := args.TaskID
	alreadyTerminal, err := st.UpdateTaskRunning(ctx, taskID)
	if err != nil {
		return fmt.Errorf("update task running: %v", err)
	}
	if alreadyTerminal {
		// The task already committed success/cancelled (a lost-ACK redelivery, or a
		// cancel that won the CAS): do NOT contact the agent. Return nil so the
		// dispatcher CompleteJob-deletes the job.
		return nil
	}

	snap, err := st.SnapshotByID(ctx, args.SnapshotID)
	if err != nil {
		return failTask(ctx, st, log, "snapshots.create", taskID, errCodeSnapshotFailed, fmt.Errorf("load snapshot: %v", err))
	}
	endpoint, vmName, err := resolveSnapshotNode(ctx, st, snap.VmID)
	if err != nil {
		return failTask(ctx, st, log, "snapshots.create", taskID, errCodeNodeUnreachable, err)
	}

	res, execErr := exec.Create(ctx, CreateExecArgs{
		VMName:             vmName,
		AdvertisedEndpoint: endpoint,
		SnapshotName:       snap.Name,
		Description:        snap.Description,
	})
	if execErr != nil {
		return failTask(ctx, st, log, "snapshots.create", taskID, errCodeSnapshotFailed, execErr)
	}

	// Project the agent-authoritative manifest (disks + vm_state_at_snapshot) and
	// write the blob reference-graph entries, then finalize the task success. The
	// projection runs first: a finalize without the manifest would leave a "success"
	// task pointing at an empty-manifest snapshot.
	if err := st.SnapshotManifestApplied(ctx, args.SnapshotID, res.Disks, res.VMStateAtSnapshot); err != nil {
		return failTask(ctx, st, log, "snapshots.create", taskID, errCodeSnapshotFailed, fmt.Errorf("apply manifest: %v", err))
	}
	result, err := json.Marshal(struct {
		SnapshotID string `json:"snapshot_id"`
	}{SnapshotID: args.SnapshotID.String()})
	if err != nil {
		result = []byte(`{}`)
	}
	if err := st.UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{ID: taskID, Status: store.TaskStatusSuccess, Result: result}); err != nil {
		return fmt.Errorf("finalize task success: %v", err)
	}
	return nil
}

func runSnapshotDelete(ctx context.Context, st WorkerStore, exec SnapshotExecutor, log *slog.Logger, args SnapshotDeleteArgs) error {
	taskID := args.TaskID
	alreadyTerminal, err := st.UpdateTaskRunning(ctx, taskID)
	if err != nil {
		return fmt.Errorf("update task running: %v", err)
	}
	if alreadyTerminal {
		return nil
	}

	// The row was soft-deleted CP-side before the task was enqueued; read it
	// including the soft-deleted state to recover the manifest digests.
	snap, err := st.SnapshotByIDIncludingDeleted(ctx, args.SnapshotID)
	if err != nil {
		return failTask(ctx, st, log, "snapshots.delete", taskID, errCodeSnapshotFailed, fmt.Errorf("load snapshot: %v", err))
	}

	digests := make([]string, 0, len(snap.Disks))
	for _, d := range snap.Disks {
		digests = append(digests, d.SHA256)
	}

	// Fail-closed GC: dereference this snapshot's blobs and learn which digests are
	// now orphaned (zero remaining refs). Only those are safe to remove; a digest
	// still referenced by another snapshot is never returned.
	orphaned, err := st.DereferenceSnapshotBlobs(ctx, args.SnapshotID, digests)
	if err != nil {
		return failTask(ctx, st, log, "snapshots.delete", taskID, errCodeSnapshotFailed, fmt.Errorf("dereference blobs: %v", err))
	}

	endpoint, vmName, err := resolveSnapshotNode(ctx, st, snap.VmID)
	if err != nil {
		return failTask(ctx, st, log, "snapshots.delete", taskID, errCodeNodeUnreachable, err)
	}

	if err := exec.Delete(ctx, DeleteExecArgs{
		VMName:             vmName,
		AdvertisedEndpoint: endpoint,
		SnapshotName:       snap.Name,
		OrphanedBlobs:      orphaned,
	}); err != nil {
		// The row stays soft-deleted; orphaned blobs leak (recoverable, reclaimable
		// by a future sweep). Fail the task so the dispatcher retries the GC.
		return failTask(ctx, st, log, "snapshots.delete", taskID, errCodeSnapshotFailed, err)
	}

	if err := st.UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{ID: taskID, Status: store.TaskStatusSuccess, Result: []byte(`{}`)}); err != nil {
		return fmt.Errorf("finalize task success: %v", err)
	}
	return nil
}

// resolveSnapshotNode resolves the VM the snapshot belongs to and the node it
// currently runs on, returning the node's advertised endpoint and the VM name the
// agent calls are keyed on. A VM with no runtime row (never started) or no current
// node has no agent to snapshot - an unreachable-node error, retryable against the
// attempt budget.
func resolveSnapshotNode(ctx context.Context, st WorkerStore, vmID uuid.UUID) (endpoint, vmName string, err error) {
	vm, err := st.VMByID(ctx, vmID)
	if err != nil {
		return "", "", fmt.Errorf("load vm: %v", err)
	}
	rt, err := st.VMRuntimeByID(ctx, vmID)
	if err != nil {
		return "", "", fmt.Errorf("load vm runtime: %v", err)
	}
	if rt.CurrentNodeID == nil {
		return "", "", fmt.Errorf("vm %s has no current node; cannot snapshot", vm.Name)
	}
	node, err := st.NodeByID(ctx, *rt.CurrentNodeID)
	if err != nil {
		return "", "", fmt.Errorf("load node: %v", err)
	}
	return node.AdvertisedEndpoint, vm.Name, nil
}

// failTask writes the terminal failed envelope and returns cause so the dispatcher
// requeues vs fails against the kind's attempt budget. Mirrors the storage-pool
// worker's failTask.
func failTask(ctx context.Context, st WorkerStore, log *slog.Logger, op string, taskID uuid.UUID, code string, cause error) error {
	envelope, marshalErr := json.Marshal(taskErrorJSON{Code: code, Message: cause.Error()})
	if marshalErr != nil {
		envelope = []byte(`{"code":"internal","message":"marshal error envelope failed"}`)
		log.ErrorContext(ctx, op+" marshal error envelope failed", "task_id", taskID, "code", code, "error", marshalErr)
	}
	if err := st.UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{ID: taskID, Status: store.TaskStatusFailed, Error: envelope}); err != nil {
		log.ErrorContext(ctx, op+" finalize-failed write failed", "task_id", taskID, "code", code, "error", err)
		return fmt.Errorf("finalize failed: %v (cause: %v)", err, cause)
	}
	return cause
}
