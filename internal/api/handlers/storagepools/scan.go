// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// Scan implements POST /v1/storage-pools/{id}/scan. Required permission:
// `storage_pool:scan` (admin / operator).
//
// The handler runs three writes in one DB
// transaction:
//
//  1. CreateTask (status=pending).
//  2. riverClient.InsertTx — enqueue the worker job.
//  3. UpdateTaskRiverJobID — stamp the weak ref so ops can drill
//     down from a task to its river_job row.
//
// Atomicity matters: a rolled-back tx leaves no orphan row in either
// place, and the client retry (whether through Idempotency-Key or a
// plain re-issue) sees a clean slate.
//
// The Idempotency-Key middleware wraps the route. The 202 response is
// recorded for replay; a retry within the 24 h TTL
// returns the same task_id verbatim, and the client polls
// /v1/tasks/{task_id} to observe whatever state the task has reached
// by replay time.
func (h *Handler) Scan(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
		return
	}

	pool, err := resolver.Pool(r.Context(), h.store.Queries(), chi.URLParam(r, "id"))
	if err != nil {
		writePoolResolveError(w, r, err, "storage pool not found", "load storage pool")
		return
	}
	poolID := pool.ID

	if !h.preflightScan(w, r, pool) {
		return
	}

	taskID, err := h.enqueueScan(r.Context(), poolID, caller.ID)
	if err != nil {
		h.log.ErrorContext(r.Context(), "storagepools.scan enqueue failed",
			"pool_id", poolID, "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "enqueue scan", nil)
		return
	}

	response.WriteJSON(w, r, http.StatusAccepted, response.AsyncTaskAccepted{
		TaskID: taskID.String(),
		Status: string(store.TaskStatusPending),
		Links:  response.AsyncTaskLinks{Self: "/v1/tasks/" + taskID.String()},
	})
}

// preflightScan validates that the pool's owning node is loadable and
// in a scannable state. The pool itself is loaded upstream by the
// resolver. Writes the 409 response directly on rejection and returns
// false; returns true to signal "go ahead, atomic enqueue is safe".
func (h *Handler) preflightScan(w http.ResponseWriter, r *http.Request, pool store.StoragePool) bool {
	node, err := h.store.Queries().GetNodeByID(r.Context(), pool.NodeID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Pool's owning node was soft-deleted between pool-load and
			// node-load (extremely rare race). Surface as 409 so the
			// client doesn't conflate it with the missing-pool 404.
			response.WriteError(w, r, http.StatusConflict,
				response.CodeConflict, "owning node is gone",
				map[string]any{"current_status": string(store.NodeStatusGone)})
			return false
		}
		h.log.ErrorContext(r.Context(), "storagepools.scan: load node failed",
			"pool_id", pool.ID, "node_id", pool.NodeID, "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load node", nil)
		return false
	}

	switch node.Status {
	case store.NodeStatusPending, store.NodeStatusReady, store.NodeStatusCordoned:
		return true
	default:
		response.WriteError(w, r, http.StatusConflict,
			response.CodeConflict, "node is not in a scannable state",
			map[string]any{"current_status": string(node.Status)})
		return false
	}
}

// enqueueScan runs the atomic three-write enqueue: insert the task row,
// hand the job to river inside the same tx, then stamp the river job
// id back onto the task. Returns the freshly-minted task id on success.
func (h *Handler) enqueueScan(ctx context.Context, poolID, callerID uuid.UUID) (uuid.UUID, error) {
	taskID := uuid.New()
	pid := poolID
	cid := callerID
	err := h.store.InTxWithTx(ctx, func(q *store.Queries, tx pgx.Tx) error {
		if _, err := q.CreateTask(ctx, store.CreateTaskParams{
			ID:           taskID,
			Type:         "storage_pool.scan",
			Status:       store.TaskStatusPending,
			ResourceType: "storage_pool",
			ResourceID:   &pid,
			Args:         []byte(`{}`),
			MaxAttempts:  25,
			CreatedBy:    &cid,
		}); err != nil {
			return err
		}

		insertResult, err := h.riverClient.InsertTx(ctx, tx,
			StoragePoolScanArgs{TaskID: taskID, PoolID: poolID}, nil)
		if err != nil {
			return err
		}

		jobID := insertResult.Job.ID
		return q.UpdateTaskRiverJobID(ctx, store.UpdateTaskRiverJobIDParams{
			ID:         taskID,
			RiverJobID: &jobID,
		})
	})
	if err != nil {
		return uuid.Nil, err
	}
	return taskID, nil
}
