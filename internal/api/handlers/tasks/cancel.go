// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package tasks

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// detailCodeNotCancellable identifies the conflict variant when a task
// is mid-execution. Surfaced via `error.details.code` (the cross-cutting
// `error.code` stays "conflict"). Spec §5.4.
const detailCodeNotCancellable = "task_not_cancellable"

// detailCodeAlreadyFinalized is the conflict variant when the task has
// already reached a terminal state. Idempotent on repeated requests.
const detailCodeAlreadyFinalized = "task_already_finalized"

// errCancelNotFound bubbles "row missing or invisible to caller" out of
// the InTxWithTx callback. The handler maps it to 404 with the no-leak
// envelope. See CLAUDE.md "403 vs 404".
var errCancelNotFound = errors.New("task not found")

// errCancelNotCancellable bubbles a running-task or
// race-lost-to-the-worker outcome out of the callback. Both surface as
// 409 with `details.code = task_not_cancellable`.
var errCancelNotCancellable = errors.New("task not cancellable")

// errCancelAlreadyFinalized bubbles a terminal-state outcome.
// Idempotent 409 with `details.code = task_already_finalized`.
var errCancelAlreadyFinalized = errors.New("task already finalized")

// taskTypeNodeDrain is the long-lived saga task type that supports a
// cooperative running-cancel via a store marker, rather than the default
// running -> 409 not-cancellable response. Mirrors the node.drain job
// Kind in internal/api/handlers/nodes.
const taskTypeNodeDrain = "node.drain"

// Cancel implements POST /v1/tasks/{id}/cancel. Required permission:
// `task:cancel`.
//
// Implements three-branch cancellation atomically inside one etcd
// transaction:
//
//   - pending → store.CancelPendingTask cancels the backing job and flips
//     the row to cancelled in one transaction → 200 with the cancelled Task
//     body. If CancelPendingTask returns store.ErrTaskNotCancellable, the
//     worker won the race and committed status=running before we did; the
//     transaction does not commit, and we return 409 task_not_cancellable.
//   - running → 409 task_not_cancellable, EXCEPT a running node.drain
//     saga, which supports a cooperative cancel: the handler sets the
//     drain cancel marker and returns 200 with the still-running Task
//     body. The saga finalizes the task to cancelled on its next poll.
//   - success / failed / cancelled → 409 task_already_finalized.
//
// The Idempotency-Key middleware wraps this handler. The 200 response
// is recorded for replay; 4xx responses (404 / 409) are not cached -
// the next retry re-evaluates state. That is the right
// behaviour for cancel: a task that became cancellable since the prior
// attempt should be cancellable on retry.
//
// RBAC scope is checked inline after the row loads: scope=own callers
// targeting another user's task get 404 (NOT 403, no existence leak).
func (h *Handler) Cancel(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
		return
	}

	taskID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "id must be a uuid", nil)
		return
	}

	cancelled, err := h.runCancel(r.Context(), caller, taskID)
	h.writeCancelResponse(w, r, taskID, cancelled, err)
}

// runCancel loads the task, applies the ownership clamp, and dispatches
// on status. The pending branch delegates to store.CancelPendingTask,
// which cancels the backing job and flips the row atomically (the queue
// seam hides the transaction). errCancel* sentinels map to envelopes in
// writeCancelResponse.
//
// The TaskByID read is intentionally non-transactional: the real atomic
// gate is CancelPendingTask's `WHERE status='pending'` conditional
// update, so a status change racing between this read and the update
// still yields ErrTaskNotCancellable (409), not a stale success.
func (h *Handler) runCancel(
	ctx context.Context,
	caller *auth.User,
	taskID uuid.UUID,
) (store.Task, error) {
	row, err := h.store.TaskByID(ctx, taskID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.Task{}, errCancelNotFound
		}
		return store.Task{}, err
	}

	// Ownership clamp. Running BEFORE the status switch: a developer
	// targeting another user's running task must still see 404, not
	// learn that the task is in flight.
	if !cancelVisible(caller, row) {
		return store.Task{}, errCancelNotFound
	}

	switch row.Status {
	case store.TaskStatusPending:
		cancelled, err := h.store.CancelPendingTask(ctx, taskID, row.JobID)
		if err != nil {
			if errors.Is(err, store.ErrTaskNotCancellable) {
				return store.Task{}, errCancelNotCancellable
			}
			return store.Task{}, err
		}
		return cancelled, nil
	case store.TaskStatusRunning:
		// A running node.drain is a long-lived saga. It cannot be
		// flipped from under the worker, but it polls a cancel marker
		// each loop: set it and let the saga finalize the task to
		// cancelled and cordon the node on its next poll. This is
		// best-effort - the response reports the task still running.
		if row.Type == taskTypeNodeDrain {
			if err := h.store.RequestDrainCancel(ctx, taskID); err != nil {
				return store.Task{}, err
			}
			return row, nil
		}
		return store.Task{}, errCancelNotCancellable
	default: // success / failed / cancelled
		return store.Task{}, errCancelAlreadyFinalized
	}
}

// cancelVisible enforces the scope=own clamp: developers / viewers
// cannot see another user's task, so a cross-user cancel attempt
// returns 404 (no existence leak) instead of 403.
func cancelVisible(caller *auth.User, row store.Task) bool {
	switch auth.ScopeFor(caller.Role, auth.PermTaskCancel) {
	case auth.ScopeAny:
		return true
	case auth.ScopeOwn:
		return row.CreatedBy != nil && *row.CreatedBy == caller.ID
	default:
		// Defensive: middleware rejects roles without task:cancel
		// before reaching here. 404 is the no-leak fall-through.
		return false
	}
}

// writeCancelResponse maps the err returned from runCancel onto an
// HTTP response. err == nil means the pending branch succeeded;
// every other branch is one of the cancel-specific sentinel errors.
func (h *Handler) writeCancelResponse(
	w http.ResponseWriter,
	r *http.Request,
	taskID uuid.UUID,
	cancelled store.Task,
	err error,
) {
	switch {
	case err == nil:
		response.WriteJSON(w, r, http.StatusOK, toView(cancelled))
	case errors.Is(err, errCancelNotFound):
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "task not found", nil)
	case errors.Is(err, errCancelNotCancellable):
		response.WriteError(w, r, http.StatusConflict,
			response.CodeConflict, "task is not cancellable in its current state",
			map[string]any{"code": detailCodeNotCancellable})
	case errors.Is(err, errCancelAlreadyFinalized):
		response.WriteError(w, r, http.StatusConflict,
			response.CodeConflict, "task has already finalized",
			map[string]any{"code": detailCodeAlreadyFinalized})
	default:
		h.log.ErrorContext(r.Context(), "tasks.cancel failed",
			"task_id", taskID, "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "cancel task", nil)
	}
}
