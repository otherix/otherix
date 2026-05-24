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

// Start handles POST /v1/vms/{vm_name}/start — async per spec.
// Returns 202 + AsyncTaskAccepted; the spawn pipeline (build args →
// qemu spawn → QMP verify → status transition) runs in а goroutine
// и progresses through the task surface (`GET /v1/tasks/{id}`).
// Idempotent: start-when-running completes the task successfully
// без re-spawning the qemu process.
func (h *Handler) Start(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "vm_name")
	if name == "" {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "vm not found", nil)
		return
	}
	task, err := h.manager.Start(r.Context(), name)
	if err != nil {
		mapAsyncLifecycleError(w, r, err)
		return
	}
	response.WriteJSON(w, r, http.StatusAccepted, asyncAccepted{
		TaskID: task.ID.String(),
		Status: string(task.Status),
		Links:  map[string]any{"self": "/v1/tasks/" + task.ID.String()},
	})
}

// mapAsyncLifecycleError translates Manager sentinel errors to the
// standard HTTP envelope for the async lifecycle surface (start /
// stop / poweroff / reboot). ErrInFlight surfaces as 409 with a
// retry_after_seconds hint mirroring the CP-side idempotency
// middleware's transient-collision shape, so a CLI / SDK client has
// a single retry contract for "operation in flight, try again
// shortly".
func mapAsyncLifecycleError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, vm.ErrNotFound):
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "vm not found", nil)
	case errors.Is(err, vm.ErrInFlight):
		response.WriteError(w, r, http.StatusConflict,
			response.CodeConflict, "vm operation already in flight",
			map[string]any{"retry_after_seconds": 1})
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
