// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/api/response"
)

// Reboot handles POST /v1/vms/{vm_name}/reboot — async per spec.
// Returns 202 + AsyncTaskAccepted; Manager.Reboot orchestrates an
// internal stop + start (Area 4-III lock — distinct от Reset; the
// QEMU process is replaced so the PID changes). On stop-phase
// timeout the task fails с code `stop_timeout`; operators dispatch
// к reset to force.
func (h *Handler) Reboot(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "vm_name")
	if name == "" {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "vm not found", nil)
		return
	}
	task, err := h.manager.Reboot(r.Context(), name)
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
