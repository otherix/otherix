// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/api/response"
)

// Stop handles POST /v1/vms/{vm_name}/stop — async per spec.
// Returns 202 + AsyncTaskAccepted; Manager.Stop runs QMP
// system_powerdown followed by WaitGone(shutdownGrace). If the
// guest does not honour the signal within the window the task
// completes as failed with code `stop_timeout` —
// no internal escalation to poweroff; operators dispatch to the
// poweroff endpoint (or `stop --force` on the CLI) to force.
func (h *Handler) Stop(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "vm_name")
	if name == "" {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "vm not found", nil)
		return
	}
	task, err := h.manager.Stop(r.Context(), name)
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
