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

// Delete handles DELETE /v1/vms/{vm_name}. Returns 202 + task_id;
// the async teardown (qmp powerdown -> wait -> SIGKILL fallback ->
// dir cleanup) runs in a goroutine and the task surface tracks
// progress.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "vm_name")
	if name == "" {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "vm not found", nil)
		return
	}
	task, err := h.manager.DeleteByName(r.Context(), name)
	if err != nil {
		if errors.Is(err, vm.ErrNotFound) {
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "vm not found", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "internal error", nil)
		return
	}
	response.WriteJSON(w, r, http.StatusAccepted, asyncAccepted{
		TaskID: task.ID.String(),
		Status: string(task.Status),
		Links:  map[string]any{"self": "/v1/tasks/" + task.ID.String()},
	})
}
