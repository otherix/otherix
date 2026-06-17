// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package snapshots

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// snapshotCreateRequest is the JSON body of POST /v1/vms/{id}/snapshots. name is
// required (1..255); description is optional. with_memory is accepted for
// forward-compat but rejected in slice A (disk-only).
type snapshotCreateRequest struct {
	Name        string  `json:"name"`
	Description *string `json:"description"`
	WithMemory  *bool   `json:"with_memory"`
}

// Create implements POST /v1/vms/{id}/snapshots - async per spec (202 +
// AsyncTaskAccepted). Required permission: snapshot:create. A caller who holds
// the capability but does not own the VM sees a 404 (no existence leak).
//
// Sequence: resolve VM by name -> ownership check (404 on deny) -> decode body
// -> reject with_memory:true (400, disk-only in slice A) -> validate name ->
// resolve vm_state_at_snapshot from the VM's observed runtime phase -> atomic
// CreateSnapshot (snapshot row + backing task) -> 202.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
		return
	}

	vm, err := resolver.VM(r.Context(), h.store, chi.URLParam(r, "id"))
	if err != nil {
		writeVMResolveError(w, r, err)
		return
	}
	if err := auth.CheckOwnership(caller, &vm.OwnerID, auth.PermSnapshotCreate); err != nil {
		writeVMNotFound(w, r)
		return
	}

	body, ok := decodeCreateBody(w, r)
	if !ok {
		return
	}

	if body.WithMemory != nil && *body.WithMemory {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "memory snapshots are not supported",
			map[string]any{"reason": "with_memory_unsupported"})
		return
	}
	if l := len(body.Name); l < 1 || l > 255 {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "name must be 1..255 characters", nil)
		return
	}

	taskID := uuid.New()
	snapshotID := uuid.New()
	resID := snapshotID
	createdBy := caller.ID

	params := store.CreateSnapshotParams{
		ID:                 snapshotID,
		VmID:               vm.ID,
		OwnerID:            vm.OwnerID,
		Name:               body.Name,
		Description:        derefString(body.Description),
		VMStateAtSnapshot:  h.vmStateAtSnapshot(r.Context(), vm.ID),
		SourceArchitecture: vm.Architecture,
		SourceFirmwareID:   vm.FirmwareID,
		Task: store.CreateTaskParams{
			ID:           taskID,
			Type:         "vm.snapshot.create",
			Status:       store.TaskStatusPending,
			ResourceType: "snapshot",
			ResourceID:   &resID,
			MaxAttempts:  25,
			CreatedBy:    &createdBy,
		},
	}

	if _, err := h.store.CreateSnapshot(r.Context(), params, SnapshotCreateArgs{TaskID: taskID, SnapshotID: snapshotID}); err != nil {
		if errors.Is(err, store.ErrSnapshotNameExists) {
			response.WriteError(w, r, http.StatusConflict,
				response.CodeConflict, "a snapshot with this name already exists for this vm", nil)
			return
		}
		h.log.ErrorContext(r.Context(), "snapshots.create create failed",
			"vm_id", vm.ID.String(), "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "create snapshot", nil)
		return
	}

	response.WriteJSON(w, r, http.StatusAccepted, response.AsyncTaskAccepted{
		TaskID: taskID.String(),
		Status: string(store.TaskStatusPending),
		Links:  response.AsyncTaskLinks{Self: "/v1/tasks/" + taskID.String()},
	})
}

// decodeCreateBody decodes the request body. A missing body is a 400 (name is
// required); a malformed body is a 400. On error it writes the envelope and
// returns ok=false.
func decodeCreateBody(w http.ResponseWriter, r *http.Request) (snapshotCreateRequest, bool) {
	var body snapshotCreateRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, "invalid request body", nil)
			return snapshotCreateRequest{}, false
		}
	}
	return body, true
}

// vmStateAtSnapshot maps the VM's observed runtime phase to the captured
// vm_state_at_snapshot. running and migrating map to running (the guest is
// live); paused stays paused; everything else (stopped / gone / error / pending /
// no runtime row) defaults to stopped - a disk-only snapshot of a non-running
// guest is crash-consistent-at-rest.
func (h *Handler) vmStateAtSnapshot(ctx context.Context, vmID uuid.UUID) store.VMStateAtSnapshot {
	rt, err := h.store.VMRuntimeByID(ctx, vmID)
	if err != nil {
		return store.VmStateAtSnapshotStopped
	}
	switch rt.Phase {
	case store.VmPhaseRunning, store.VmPhaseMigrating:
		return store.VmStateAtSnapshotRunning
	case store.VmPhasePaused:
		return store.VmStateAtSnapshotPaused
	default:
		return store.VmStateAtSnapshotStopped
	}
}
