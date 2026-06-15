// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/api/response"
)

// Reset handles POST /v1/vms/{vm_name}/reset. Synchronous per Pre-L1
// reset spec amendment: the agent dials QMP, issues `system_reset`,
// returns the refreshed VM view. The QEMU process keeps running and
// the guest CPU is reset; persisted phase stays running because the
// runtime identity is preserved — operators detect the reboot via
// guest uptime, not via CP state.
func (h *Handler) Reset(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "vm_name")
	if name == "" {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "vm not found", nil)
		return
	}
	v, err := h.manager.Reset(r.Context(), name)
	if err != nil {
		mapLifecycleError(w, r, err)
		return
	}
	response.WriteJSON(w, r, http.StatusOK, toView(v, 0))
}
