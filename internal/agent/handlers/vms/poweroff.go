// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/api/response"
)

// Poweroff handles POST /v1/vms/{vm_name}/poweroff — async per spec.
// Returns 202 + AsyncTaskAccepted; Manager.Poweroff attempts QMP
// `quit` (clean socket / pidfile release) и falls back к SIGKILL
// after poweroffGrace (5s). The guest OS is not notified per spec.
func (h *Handler) Poweroff(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "vm_name")
	if name == "" {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "vm not found", nil)
		return
	}
	task, err := h.manager.Poweroff(r.Context(), name)
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
