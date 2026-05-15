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

// Get handles GET /v1/vms/{vm_name}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "vm_name")
	if name == "" {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "vm not found", nil)
		return
	}
	v, err := h.manager.GetByName(name)
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
	response.WriteJSON(w, r, http.StatusOK, toView(v))
}
