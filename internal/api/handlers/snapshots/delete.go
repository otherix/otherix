// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package snapshots

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// Delete implements DELETE /v1/snapshots/{id} - async per spec (202 +
// AsyncTaskAccepted). Required permission: snapshot:delete. A non-owner gets 404
// (no existence leak).
//
// Sequence: resolve snapshot by id -> ownership check (404 on deny) ->
// store.DeleteSnapshot (sync, fail-closed: refuses a snapshot with live children
// with 409 snapshot_has_children, refuses with 409 + blocking_resources while an
// in-flight vm.create is sourcing it, else soft-deletes + flips to deleting) ->
// enqueue a vm.snapshot.delete task for the async blob GC -> 202. The blob
// removal itself is the worker's job, gated on the CP reference graph.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
		return
	}

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "id must be a uuid", nil)
		return
	}

	snap, err := h.store.SnapshotByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeSnapshotNotFound(w, r)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load snapshot", nil)
		return
	}

	if err := auth.CheckOwnership(caller, &snap.OwnerID, auth.PermSnapshotDelete); err != nil {
		writeSnapshotNotFound(w, r)
		return
	}

	// Soft-delete the row AND enqueue the blob-GC task+job in ONE transaction
	// (store-side), so a CP crash cannot leave a soft-deleted snapshot whose blobs
	// have no reclaimer (a retried DELETE would 404 on the already-deleted row).
	taskID := uuid.New()
	resID := id
	createdBy := caller.ID
	taskParams := store.CreateTaskParams{
		ID:           taskID,
		Type:         "vm.snapshot.delete",
		Status:       store.TaskStatusPending,
		ResourceType: "snapshot",
		ResourceID:   &resID,
		MaxAttempts:  25,
		CreatedBy:    &createdBy,
	}
	if _, err := h.store.DeleteSnapshot(r.Context(), id, taskParams, SnapshotDeleteArgs{TaskID: taskID, SnapshotID: id}); err != nil {
		if errors.Is(err, store.ErrSnapshotHasChildren) {
			response.WriteError(w, r, http.StatusConflict,
				response.CodeConflict, "snapshot has non-deleted children",
				map[string]any{"reason": "snapshot_has_children"})
			return
		}
		if errors.Is(err, store.ErrSnapshotSourcingCreate) {
			response.WriteBlockingResources(w, r, &response.BlockingResourcesError{
				Message:   "snapshot is being sourced by an in-flight VM create; wait for it to finish or delete that VM first",
				Resources: map[string]int64{"vm_creates": 1},
			})
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			writeSnapshotNotFound(w, r)
			return
		}
		h.log.ErrorContext(r.Context(), "snapshots.delete failed",
			"snapshot_id", id.String(), "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "delete snapshot", nil)
		return
	}

	response.WriteJSON(w, r, http.StatusAccepted, response.AsyncTaskAccepted{
		TaskID: taskID.String(),
		Status: string(store.TaskStatusPending),
		Links:  response.AsyncTaskLinks{Self: "/v1/tasks/" + taskID.String()},
	})
}
