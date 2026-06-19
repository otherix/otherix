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

// Broker pulls a blob to a consumer node via the CP-brokered pull saga. The
// production implementation is *blobbroker.Broker (wired in cmd/api); the
// interface keeps this package free of a blobbroker import.
type Broker interface {
	BrokerPull(ctx context.Context, digest string, consumerNodeID uuid.UUID) error
}

// ReplicateWorkerStore is the task-mutator surface the replicate worker needs.
// *etcdstore.Store satisfies it.
type ReplicateWorkerStore interface {
	UpdateTaskRunning(ctx context.Context, id uuid.UUID) (alreadyTerminal bool, err error)
	UpdateTaskFinalized(ctx context.Context, arg store.UpdateTaskFinalizedParams) error
	EndReplicate(ctx context.Context, digest string, nodeID uuid.UUID) error
}

const errCodeReplicateFailed = "replicate_failed"

// ReplicateHandler returns the dispatcher handler for artifact.replicate jobs: it
// pulls one blob to one target node via the pull saga and finalizes the task. A
// pull failure finalizes the task failed AND returns the cause so the dispatcher
// requeues against the kind's attempt budget; the reconcile loop also re-enqueues
// on its next pass while the target is short, so a replica is retried until it
// lands (or no live holder remains).
func ReplicateHandler(st ReplicateWorkerStore, broker Broker, log *slog.Logger) func(context.Context, []byte) error {
	return func(ctx context.Context, raw []byte) error {
		var args ReplicateArgs
		if err := json.Unmarshal(raw, &args); err != nil {
			return fmt.Errorf("unmarshal artifact.replicate args: %v", err)
		}
		return runReplicate(ctx, st, broker, log, args)
	}
}

func runReplicate(ctx context.Context, st ReplicateWorkerStore, broker Broker, log *slog.Logger, args ReplicateArgs) error {
	alreadyTerminal, err := st.UpdateTaskRunning(ctx, args.TaskID)
	if err != nil {
		return fmt.Errorf("update task running: %v", err)
	}
	if alreadyTerminal {
		// A lost-ACK redelivery or a cancel that won the CAS: do not pull. Return
		// nil so the dispatcher deletes the job. This path owns no in-flight marker.
		return nil
	}
	defer func() {
		if err := st.EndReplicate(ctx, args.Digest, args.TargetNodeID); err != nil {
			log.WarnContext(ctx, "artifact.replicate: clear in-flight marker failed",
				slog.String("digest", args.Digest), slog.String("node_id", args.TargetNodeID.String()), slog.Any("error", err))
		}
	}()
	if err := broker.BrokerPull(ctx, args.Digest, args.TargetNodeID); err != nil {
		return failReplicate(ctx, st, log, args.TaskID, err)
	}
	if err := st.UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{
		ID:     args.TaskID,
		Status: store.TaskStatusSuccess,
		Result: []byte(`{}`),
	}); err != nil {
		return fmt.Errorf("finalize replicate success: %v", err)
	}
	return nil
}

// failReplicate writes the terminal failed envelope and returns cause so the
// dispatcher requeues vs fails against the attempt budget.
func failReplicate(ctx context.Context, st ReplicateWorkerStore, log *slog.Logger, taskID uuid.UUID, cause error) error {
	envelope, marshalErr := json.Marshal(struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: errCodeReplicateFailed, Message: cause.Error()})
	if marshalErr != nil {
		envelope = []byte(`{"code":"internal","message":"marshal error envelope failed"}`)
	}
	if err := st.UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{ID: taskID, Status: store.TaskStatusFailed, Error: envelope}); err != nil {
		log.ErrorContext(ctx, "artifact.replicate finalize-failed write failed", slog.String("task_id", taskID.String()), slog.Any("error", err))
		return fmt.Errorf("finalize failed: %v (cause: %v)", err, cause)
	}
	return cause
}
