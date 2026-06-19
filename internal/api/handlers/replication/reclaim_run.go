// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package replication

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// ReclaimWorkerStore is the read + task-mutator surface the reclaim worker needs
// to re-derive safety at execution and finalize the task. *etcdstore.Store
// satisfies it.
type ReclaimWorkerStore interface {
	DurabilityStore
	UpdateTaskRunning(ctx context.Context, id uuid.UUID) (alreadyTerminal bool, err error)
	UpdateTaskFinalized(ctx context.Context, arg store.UpdateTaskFinalizedParams) error
	EndReclaim(ctx context.Context, digest string, nodeID uuid.UUID) error
}

const errCodeReclaimFailed = "reclaim_failed"

// ReclaimHandler returns the dispatcher handler for artifact.reclaim jobs: it
// deletes one surplus blob copy from one node and finalizes the task. Before
// touching the agent it re-derives the desired replica count and the live
// holders freshly: a reclaim proceeds only if removing the target still leaves at
// least the desired number of live holders. A concurrent re-reference (an
// orphaned blob recreated into a VM) raises desired and the task aborts toward
// inaction - the destructive delete never fires on stale state.
func ReclaimHandler(st ReclaimWorkerStore, reclaimer Reclaimer, log *slog.Logger) func(context.Context, []byte) error {
	return func(ctx context.Context, raw []byte) error {
		var args ReclaimArgs
		if err := json.Unmarshal(raw, &args); err != nil {
			return fmt.Errorf("unmarshal artifact.reclaim args: %v", err)
		}
		return runReclaim(ctx, st, reclaimer, log, args)
	}
}

func runReclaim(ctx context.Context, st ReclaimWorkerStore, reclaimer Reclaimer, log *slog.Logger, args ReclaimArgs) error {
	alreadyTerminal, err := st.UpdateTaskRunning(ctx, args.TaskID)
	if err != nil {
		return fmt.Errorf("update task running: %v", err)
	}
	if alreadyTerminal {
		// A lost-ACK redelivery or a cancel that won the CAS: do not delete. Return
		// nil so the dispatcher deletes the job. This path owns no in-flight marker.
		return nil
	}
	defer func() {
		if err := st.EndReclaim(ctx, args.Digest, args.TargetNodeID); err != nil {
			log.WarnContext(ctx, "artifact.reclaim: clear in-flight marker failed",
				slog.String("digest", args.Digest), slog.String("node_id", args.TargetNodeID.String()), slog.Any("error", err))
		}
	}()

	safe, err := reclaimStillSafe(ctx, st, args.Digest, args.TargetNodeID)
	if err != nil {
		return failReclaim(ctx, st, log, args.TaskID, err)
	}
	if !safe {
		log.InfoContext(ctx, "artifact.reclaim: aborted, blob still needed or already gone",
			slog.String("digest", args.Digest), slog.String("node_id", args.TargetNodeID.String()))
		return finalizeReclaimSuccess(ctx, st, args.TaskID)
	}

	if err := reclaimer.Reclaim(ctx, args.TargetNodeID, args.Digest); err != nil {
		return failReclaim(ctx, st, log, args.TaskID, err)
	}
	return finalizeReclaimSuccess(ctx, st, args.TaskID)
}

// reclaimStillSafe re-derives the desired replica count and the live holders and
// reports whether deleting target is still justified: target must currently hold
// the blob, and removing it must leave at least desired live holders. desired is
// 0 for an orphaned blob (zero snapshot refs) and K otherwise.
func reclaimStillSafe(ctx context.Context, st ReclaimWorkerStore, digest string, target uuid.UUID) (bool, error) {
	nodes, err := st.AllNodes(ctx)
	if err != nil {
		return false, fmt.Errorf("list nodes: %v", err)
	}
	live := liveNodeIDs(nodes)
	refs, err := st.SnapshotsReferencingBlob(ctx, digest)
	if err != nil {
		return false, fmt.Errorf("list refs: %v", err)
	}
	desired := 0
	if len(refs) > 0 {
		k, _, err := blobPlacementTarget(ctx, st, digest, nodes, live)
		if err != nil {
			return false, err
		}
		desired = k
	}
	holders, err := liveHolders(ctx, st, digest, live)
	if err != nil {
		return false, err
	}
	holds := false
	for _, h := range holders {
		if h == target {
			holds = true
			break
		}
	}
	if !holds {
		return false, nil
	}
	return len(holders)-1 >= desired, nil
}

func finalizeReclaimSuccess(ctx context.Context, st ReclaimWorkerStore, taskID uuid.UUID) error {
	if err := st.UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{
		ID:     taskID,
		Status: store.TaskStatusSuccess,
		Result: []byte(`{}`),
	}); err != nil {
		return fmt.Errorf("finalize reclaim success: %v", err)
	}
	return nil
}

// failReclaim writes the terminal failed envelope and returns cause so the
// dispatcher requeues vs fails against the attempt budget. The agent reclaim is
// idempotent, so a retry of a partially-applied delete is safe.
func failReclaim(ctx context.Context, st ReclaimWorkerStore, log *slog.Logger, taskID uuid.UUID, cause error) error {
	envelope, marshalErr := json.Marshal(struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: errCodeReclaimFailed, Message: cause.Error()})
	if marshalErr != nil {
		envelope = []byte(`{"code":"internal","message":"marshal error envelope failed"}`)
	}
	if err := st.UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{ID: taskID, Status: store.TaskStatusFailed, Error: envelope}); err != nil {
		log.ErrorContext(ctx, "artifact.reclaim finalize-failed write failed", slog.String("task_id", taskID.String()), slog.Any("error", err))
		return fmt.Errorf("finalize failed: %v (cause: %v)", err, cause)
	}
	return cause
}
