// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/agent/vm"
	"github.com/otherix/otherix/internal/agentapi"
	"github.com/otherix/otherix/internal/api/response"
)

// SnapshotCreate handles POST /v1/vms/{vm_name}/snapshots - async (202). The
// manager captures each VM disk into a content-addressed blob and assembles a
// manifest; the terminal-success task result carries the agentapi.Snapshot the
// CP worker decodes. (vm, snapshot_name) idempotency is enforced manager-side:
// a redelivered create returns the original task without re-capturing.
func (h *Handler) SnapshotCreate(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "vm_name")
	if name == "" {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "vm not found", nil)
		return
	}

	var req agentapi.SnapshotCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "request body is not valid JSON",
			map[string]any{"err": err.Error()})
		return
	}
	if req.Name == "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "name is required", nil)
		return
	}

	spec := vm.SnapshotSpec{Name: req.Name}
	if req.Description != nil {
		spec.Description = *req.Description
	}
	if req.WithMemory != nil {
		spec.WithMemory = *req.WithMemory
	}

	task, err := h.manager.CreateSnapshot(r.Context(), name, spec)
	if err != nil {
		mapSnapshotError(w, r, err)
		return
	}
	response.WriteJSON(w, r, http.StatusAccepted, asyncAccepted{
		TaskID: task.ID.String(),
		Status: string(task.Status),
		Links:  map[string]any{"self": "/v1/tasks/" + task.ID.String()},
	})
}

// SnapshotsList handles GET /v1/vms/{vm_name}/snapshots - sync (200). Returns
// the materialized snapshots for the VM as a bare array of agentapi.Snapshot
// (empty array, not null, when none exist).
func (h *Handler) SnapshotsList(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "vm_name")
	snaps, err := h.manager.VMSnapshots(name)
	if err != nil {
		mapSnapshotError(w, r, err)
		return
	}
	response.WriteJSON(w, r, http.StatusOK, snaps)
}

// SnapshotGet handles GET /v1/vms/{vm_name}/snapshots/{snapshot_name} - sync
// (200). A missing snapshot is 404; the agent never leaks "exists for another
// VM".
func (h *Handler) SnapshotGet(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "vm_name")
	snapName := chi.URLParam(r, "snapshot_name")
	snap, ok, err := h.manager.VMSnapshot(name, snapName)
	if err != nil {
		mapSnapshotError(w, r, err)
		return
	}
	if !ok {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "snapshot not found", nil)
		return
	}
	response.WriteJSON(w, r, http.StatusOK, snap)
}

// SnapshotDelete handles DELETE /v1/vms/{vm_name}/snapshots/{snapshot_name} -
// async (202). The manager removes the manifest and fail-closed GCs the blobs
// no other local manifest references. The CP-passed `orphaned` query params
// are advisory only: the agent's local reference scan is authoritative for
// what it owns and never deletes a blob still referenced locally.
func (h *Handler) SnapshotDelete(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "vm_name")
	snapName := chi.URLParam(r, "snapshot_name")
	task, err := h.manager.DeleteSnapshot(r.Context(), name, snapName)
	if err != nil {
		mapSnapshotError(w, r, err)
		return
	}
	response.WriteJSON(w, r, http.StatusAccepted, asyncAccepted{
		TaskID: task.ID.String(),
		Status: string(task.Status),
		Links:  map[string]any{"self": "/v1/tasks/" + task.ID.String()},
	})
}

// Revert handles POST /v1/vms/{vm_name}/revert. In-place revert-from-snapshot
// is out of slice-A scope (recreate-from-snapshot lands in Task 12); the route
// is bound so the surface exists, returning 501 not_implemented.
func (h *Handler) Revert(w http.ResponseWriter, r *http.Request) {
	response.WriteError(w, r, http.StatusNotImplemented,
		response.CodeNotImplemented, "in-place snapshot revert is not implemented", nil)
}

// mapSnapshotError translates manager snapshot errors to the standard
// envelope. An unknown VM or pool is 404 (never leak existence); with_memory
// and an invalid spec are 400 validation_failed; everything else is internal.
func mapSnapshotError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, vm.ErrNotFound), errors.Is(err, vm.ErrPoolUnknown):
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "vm not found", nil)
	case errors.Is(err, vm.ErrSnapshotWithMemory), errors.Is(err, vm.ErrInvalidSpec):
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, err.Error(), nil)
	default:
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "internal error", nil)
	}
}
