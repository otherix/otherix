// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/agent/vm"
	"github.com/otherix/otherix/internal/api/response"
)

// Pause handles POST /v1/vms/{vm_name}/pause. Synchronous:
// the agent dials QMP, issues `stop`, returns the refreshed
// VM view with phase=paused. Maps Manager errors:
//
//   - ErrNotFound       → 404 not_found
//   - ErrInvalidState   → 409 conflict (e.g. pause-when-stopped)
//   - ErrQMPUnavailable → 500 internal
func (h *Handler) Pause(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "vm_name")
	if name == "" {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "vm not found", nil)
		return
	}
	v, err := h.manager.Pause(r.Context(), name)
	if err != nil {
		mapLifecycleError(w, r, err)
		return
	}
	response.WriteJSON(w, r, http.StatusOK, toView(v, 0))
}

// mapLifecycleError translates Manager sentinel errors to the
// standard HTTP envelope. Shared by Pause / Resume / Reset.
func mapLifecycleError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, vm.ErrNotFound):
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "vm not found", nil)
	case errors.Is(err, vm.ErrInvalidState):
		response.WriteError(w, r, http.StatusConflict,
			response.CodeConflict, err.Error(), nil)
	case errors.Is(err, vm.ErrQMPUnavailable):
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "qmp command failed", nil)
	default:
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "internal error", nil)
	}
}
