// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package firmwares

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/store"
)

// Get implements GET /v1/firmwares/{id}. Required permission:
// firmware:read.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "id must be a uuid", nil)
		return
	}

	row, err := h.store.FirmwareByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "firmware not found", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load firmware", nil)
		return
	}

	response.WriteJSON(w, r, http.StatusOK, toView(row))
}
