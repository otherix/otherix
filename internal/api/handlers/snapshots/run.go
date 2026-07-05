// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package snapshots

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/agentclient"
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
	// TaskByID reads the backing task so the create worker can resume on a
	// persisted agent_task_id instead of re-POSTing on a redelivery.
	TaskByID(ctx context.Context, id uuid.UUID) (store.Task, error)
	// UpdateTaskAgentTaskID stamps the agent-side task id onto the task after the
	// one-shot PostSnapshot, before polling: a crash mid-poll resumes Poll on the
	// persisted id rather than re-POSTing a second capture (the resume seam,
	// mirroring the migrations worker).
	UpdateTaskAgentTaskID(ctx context.Context, arg store.UpdateTaskAgentTaskIDParams) error
	// ClearTaskAgentTaskID nils the persisted agent task id so the next redelivery
	// re-POSTs instead of polling a vanished agent task forever. Used when Poll
	// returns a permanent 404 (the agent restarted and lost its in-memory task);
	// the agent's (vm, snapshot_name) capture idempotency reconciles the re-POST.
	ClearTaskAgentTaskID(ctx context.Context, id uuid.UUID) error
	SnapshotByID(ctx context.Context, id uuid.UUID) (store.Snapshot, error)
	// SnapshotByIDIncludingDeleted reads a snapshot row even when soft-deleted - the
	// delete worker runs AFTER the row is soft-deleted CP-side, so it needs the
	// manifest digests off the deleted row to drive the blob dereference.
	SnapshotByIDIncludingDeleted(ctx context.Context, id uuid.UUID) (store.Snapshot, error)
	VMByID(ctx context.Context, id uuid.UUID) (store.VM, error)
	VMRuntimeByID(ctx context.Context, vmID uuid.UUID) (store.VMRuntime, error)
	NodeByID(ctx context.Context, id uuid.UUID) (store.Node, error)
	// SnapshotManifestApplied projects the agent-reported manifest (disks +
	// vm_state_at_snapshot), writes the blob reference-graph entries, and seeds the
	// placement map under nodeID (the producing node) in one Txn.
	SnapshotManifestApplied(ctx context.Context, id, nodeID uuid.UUID, disks []store.SnapshotDisk, vmState store.VMStateAtSnapshot) error
	// DereferenceSnapshotBlobs removes this snapshot's reference-graph entries for
	// the given digests and returns ONLY the digests left with zero remaining refs
	// (the orphaned set safe to GC). Fail-closed: a digest still referenced by
	// another snapshot is never returned.
	DereferenceSnapshotBlobs(ctx context.Context, snapshotID uuid.UUID, digests []string) (orphaned []string, err error)
	// BlobPlacements returns the node ids holding the blob with the given digest. The
	// delete worker resolves a snapshot's holder node(s) through it - NOT through the
	// source VM - so deletion works after the VM is gone. Entries are durable (no
	// remove path): the best-effort delete cannot confirm a blob is physically gone,
	// so pruning the map is the blob-reconcile loop's job, not the delete path's.
	BlobPlacements(ctx context.Context, digest string) ([]uuid.UUID, error)
	// MarkSnapshotError surfaces a capture failure onto the snapshot row (flips a
	// still-creating row to status=error + sets error_message). Best-effort and
	// narrow: a no-op on an absent, soft-deleted, or already-terminal row, so it
	// never resurrects a deleted row or clobbers a committed success. Called only
	// on the CREATE-kind failure path - the delete path leaves the row soft-deleted.
	MarkSnapshotError(ctx context.Context, id uuid.UUID, msg string) error
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
		return failCreateTask(ctx, st, log, "snapshots.create", taskID, args.SnapshotID, errCodeSnapshotFailed, fmt.Errorf("load snapshot: %v", err))
	}
	endpoint, vmName, nodeID, err := resolveSnapshotNode(ctx, st, snap.VmID)
	if err != nil {
		return failCreateTask(ctx, st, log, "snapshots.create", taskID, args.SnapshotID, errCodeNodeUnreachable, err)
	}

	task, err := st.TaskByID(ctx, taskID)
	if err != nil {
		return fmt.Errorf("reload task: %v", err)
	}

	res, execErr := startOrResumeCapture(ctx, st, exec, taskID, task, CreateExecArgs{
		VMName:             vmName,
		AdvertisedEndpoint: endpoint,
		SnapshotName:       snap.Name,
		Description:        snap.Description,
	})
	if execErr != nil {
		// A permanent agent-task 404 means the agent restarted and lost the in-memory
		// task we were polling. Clear the persisted agent_task_id so the redelivery
		// re-POSTs (the agent's (vm, snapshot_name) capture idempotency reconciles it)
		// instead of polling a vanished task to a hard failure forever. Mirrors the
		// vm.create worker's clearAgentTaskIDIfGone.
		if agentTaskGone(execErr) {
			if cerr := st.ClearTaskAgentTaskID(ctx, taskID); cerr != nil {
				return fmt.Errorf("clear agent_task_id: %v", cerr)
			}
		}
		return failCreateTask(ctx, st, log, "snapshots.create", taskID, args.SnapshotID, errCodeSnapshotFailed, execErr)
	}

	// Project the agent-authoritative manifest (disks + vm_state_at_snapshot) and
	// write the blob reference-graph entries, then finalize the task success. The
	// projection runs first: a finalize without the manifest would leave a "success"
	// task pointing at an empty-manifest snapshot.
	if err := st.SnapshotManifestApplied(ctx, args.SnapshotID, nodeID, res.Disks, res.VMStateAtSnapshot); err != nil {
		if errors.Is(err, store.ErrConcurrentUpdate) {
			// Transient CAS contention on the snapshot row (a concurrent delete
			// racing this projection): retry the job rather than failing a capture
			// that actually succeeded on the agent.
			return fmt.Errorf("apply manifest (retryable): %w", err)
		}
		return failCreateTask(ctx, st, log, "snapshots.create", taskID, args.SnapshotID, errCodeSnapshotFailed, fmt.Errorf("apply manifest: %v", err))
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

// startOrResumeCapture drives the agent capture with agent_task_id resumption,
// mirroring the migrations worker's startOrResume. When the task already carries
// an agent task id (a redelivery while the capture is still running - worker crash
// mid-poll or a finalize-failed retry), it SKIPS the agent POST and resumes Poll
// on the persisted id; a redelivered job must not double-POST a second full-disk
// blockdev-backup (which, if the disk changed, would yield different blob digests
// and orphan the first attempt's refs). Otherwise it POSTs once, persists the
// returned id BEFORE polling (so a crash mid-poll resumes rather than re-POSTs),
// then polls.
//
// Residual window: a crash between PostSnapshot returning the id and
// UpdateTaskAgentTaskID committing it leaves the task without a persisted id, so a
// redelivery re-POSTs. That CP-side window is closed agent-side by (vm,
// snapshot_name) capture idempotency.
func startOrResumeCapture(ctx context.Context, st WorkerStore, exec SnapshotExecutor, taskID uuid.UUID, task store.Task, args CreateExecArgs) (CreateExecResult, error) {
	if task.AgentTaskID != nil {
		return exec.Poll(ctx, args.AdvertisedEndpoint, *task.AgentTaskID)
	}

	agentTaskID, err := exec.Post(ctx, args)
	if err != nil {
		return CreateExecResult{}, err
	}
	if err := st.UpdateTaskAgentTaskID(ctx, store.UpdateTaskAgentTaskIDParams{ID: taskID, AgentTaskID: &agentTaskID}); err != nil {
		return CreateExecResult{}, fmt.Errorf("persist agent_task_id: %v", err)
	}
	return exec.Poll(ctx, args.AdvertisedEndpoint, agentTaskID)
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

	// Resolve the snapshot's holder node(s) through the PLACEMENT map, NOT the source
	// VM: a standalone snapshot must stay deletable after its VM is gone. Discover
	// over ALL the snapshot's digests (not just the orphaned ones) so the manifest's
	// producing node - which holds every digest - is always contacted to drop the
	// manifest, even when every blob is shared (nothing orphaned).
	placements, holders, err := resolveSnapshotHolders(ctx, st, digests)
	if err != nil {
		return failTask(ctx, st, log, "snapshots.delete", taskID, errCodeSnapshotFailed, err)
	}
	if len(holders) == 0 {
		// No placement seed (a pre-placement-map snapshot, or a zero-disk row): the
		// holder node cannot be resolved VM-independently. Fail toward inaction - the
		// row is already soft-deleted; any blob leak is recoverable by a future sweep,
		// and wedging the task forever is worse than the leak.
		log.WarnContext(ctx, "snapshot delete: no placement entries; cannot locate blobs, leaking (fail-open)",
			"task_id", taskID, "snapshot_id", args.SnapshotID, "vm_id", snap.VmID)
		if err := st.UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{ID: taskID, Status: store.TaskStatusSuccess, Result: []byte(`{}`)}); err != nil {
			return fmt.Errorf("finalize task success: %v", err)
		}
		return nil
	}

	if err := deleteSnapshotOnHolders(ctx, st, exec, log, taskID, snap, orphaned, placements, holders); err != nil {
		return err
	}

	// Placement entries are intentionally NOT removed here. The agent delete is
	// best-effort and fire-and-forget (and fail-closed-leaks on any uncertainty),
	// so an unconfirmed "success" cannot prove the blob is gone; dropping the entry
	// would discard the only VM-independent pointer to a possibly-leaked blob. The
	// map stays as a conservative reclamation index that the blob-reconcile loop
	// prunes after verifying blob absence per node.
	if err := st.UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{ID: taskID, Status: store.TaskStatusSuccess, Result: []byte(`{}`)}); err != nil {
		return fmt.Errorf("finalize task success: %v", err)
	}
	return nil
}

// deleteSnapshotOnHolders drives the per-holder-node agent delete (manifest + the
// orphaned blobs that node holds). A holder whose node row is gone (NodeByID
// ErrNotFound = permanently decommissioned) is unreachable - skip it (recoverable
// leak; its placement entry stays as a reclamation pointer), do not wedge. A
// reachable node whose agent delete fails transiently fails the task so the
// dispatcher retries (the agent delete is idempotent: a re-run over an
// already-removed manifest/blob succeeds). Returns a failTask result (terminal
// already written) on failure, or nil when every holder was handled.
func deleteSnapshotOnHolders(ctx context.Context, st WorkerStore, exec SnapshotExecutor, log *slog.Logger, taskID uuid.UUID, snap store.Snapshot, orphaned []string, placements map[string][]uuid.UUID, holders []uuid.UUID) error {
	for _, nodeID := range holders {
		node, nerr := st.NodeByID(ctx, nodeID)
		if nerr != nil {
			if errors.Is(nerr, store.ErrNotFound) {
				log.WarnContext(ctx, "snapshot delete: holder node gone; skipping (blobs unreachable, leaked)",
					"task_id", taskID, "snapshot_id", snap.ID, "node_id", nodeID)
				continue
			}
			return failTask(ctx, st, log, "snapshots.delete", taskID, errCodeNodeUnreachable, fmt.Errorf("load node %s: %v", nodeID, nerr))
		}
		if err := exec.Delete(ctx, DeleteExecArgs{
			VMID:               snap.VmID,
			AdvertisedEndpoint: node.AdvertisedEndpoint,
			SnapshotName:       snap.Name,
			OrphanedBlobs:      orphansOnNode(orphaned, placements, nodeID),
		}); err != nil {
			// The row stays soft-deleted; orphaned blobs leak (recoverable, reclaimable
			// by a future sweep). Fail the task so the dispatcher retries the GC.
			return failTask(ctx, st, log, "snapshots.delete", taskID, errCodeSnapshotFailed, err)
		}
	}
	return nil
}

// resolveSnapshotHolders reads the placement map for every digest the snapshot
// references and returns (per-digest holders, deduped holder-node union). The union
// drives the per-node agent deletes; the per-digest map lets orphansOnNode hand each
// node only the orphaned digests it actually holds. It never touches the source VM.
func resolveSnapshotHolders(ctx context.Context, st WorkerStore, digests []string) (map[string][]uuid.UUID, []uuid.UUID, error) {
	placements := make(map[string][]uuid.UUID, len(digests))
	var holders []uuid.UUID
	seen := map[uuid.UUID]bool{}
	for _, digest := range digests {
		nodes, err := st.BlobPlacements(ctx, digest)
		if err != nil {
			return nil, nil, fmt.Errorf("read placement %q: %v", digest, err)
		}
		placements[digest] = nodes
		for _, n := range nodes {
			if !seen[n] {
				seen[n] = true
				holders = append(holders, n)
			}
		}
	}
	return placements, holders, nil
}

// orphansOnNode returns the orphaned digests held on nodeID, so a holder node is
// asked to GC only the blobs it actually stores (at K=1 every orphan maps to the
// single producing node; the filter generalises to the dedup-induced multi-holder
// case without changing the K=1 behaviour).
func orphansOnNode(orphaned []string, placements map[string][]uuid.UUID, nodeID uuid.UUID) []string {
	var out []string
	for _, digest := range orphaned {
		for _, n := range placements[digest] {
			if n == nodeID {
				out = append(out, digest)
				break
			}
		}
	}
	return out
}

// agentTaskGone reports whether err is a permanent agent-task 404 (the agent
// restarted and lost the in-memory task), the signal to clear the persisted
// agent_task_id and re-POST on redelivery. Mirrors the vm-worker's isAgentTaskGone.
func agentTaskGone(err error) bool {
	var ae *agentclient.AgentError
	return errors.As(err, &ae) && ae.Status == 404
}

// resolveSnapshotNode resolves the VM the snapshot belongs to and the node it
// currently runs on, returning the node's advertised endpoint, the VM name the
// agent calls are keyed on, and the node id (recorded in the placement map). A VM
// with no runtime row (never started) or no current node has no agent to snapshot -
// an unreachable-node error, retryable against the attempt budget. Used by the
// CREATE path only; delete resolves the node through the placement map instead.
func resolveSnapshotNode(ctx context.Context, st WorkerStore, vmID uuid.UUID) (endpoint, vmName string, nodeID uuid.UUID, err error) {
	vm, err := st.VMByID(ctx, vmID)
	if err != nil {
		return "", "", uuid.Nil, fmt.Errorf("load vm: %v", err)
	}
	rt, err := st.VMRuntimeByID(ctx, vmID)
	if err != nil {
		return "", "", uuid.Nil, fmt.Errorf("load vm runtime: %v", err)
	}
	if rt.CurrentNodeID == nil {
		return "", "", uuid.Nil, fmt.Errorf("vm %s has no current node; cannot snapshot", vm.Name)
	}
	node, err := st.NodeByID(ctx, *rt.CurrentNodeID)
	if err != nil {
		return "", "", uuid.Nil, fmt.Errorf("load node: %v", err)
	}
	return node.AdvertisedEndpoint, vm.Name, node.ID, nil
}

// failCreateTask is the CREATE-kind failure path: it fails the backing task (the
// authoritative signal) AND, best-effort, surfaces the failure onto the snapshot
// row so an operator listing snapshots sees status=error + the reason rather than a
// permanently-creating row. MarkSnapshotError is narrow (a no-op on an
// absent/soft-deleted/already-terminal row) so this never resurrects or clobbers;
// it is purely a nicety, so a MarkSnapshotError error is logged and swallowed -
// the task failure stands. The delete path must NOT call this (it leaves the row
// soft-deleted).
func failCreateTask(ctx context.Context, st WorkerStore, log *slog.Logger, op string, taskID, snapshotID uuid.UUID, code string, cause error) error {
	failErr := failTask(ctx, st, log, op, taskID, code, cause)
	if err := st.MarkSnapshotError(ctx, snapshotID, code+": "+cause.Error()); err != nil {
		log.WarnContext(ctx, op+" mark snapshot error failed (row stays creating; task failure is authoritative)",
			"task_id", taskID, "snapshot_id", snapshotID, "error", err)
	}
	return failErr
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
