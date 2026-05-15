// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/api/response"
)

// Resume handles POST /v1/vms/{vm_name}/resume. Synchronous per L1
// scope: the agent dials QMP, issues `cont`, returns the refreshed
// VM view с phase=running.
func (h *Handler) Resume(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "vm_name")
	if name == "" {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "vm not found", nil)
		return
	}
	v, err := h.manager.Resume(r.Context(), name)
	if err != nil {
		mapLifecycleError(w, r, err)
		return
	}
	response.WriteJSON(w, r, http.StatusOK, toView(v))
}
