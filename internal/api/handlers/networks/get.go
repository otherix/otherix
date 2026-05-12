// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package networks

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/otherix/otherix/internal/api/response"
)

// Get implements GET /v1/networks/{id}. Required permission:
// network:read.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "id must be a uuid", nil)
		return
	}

	row, err := h.store.Queries().GetNetworkByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "network not found", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load network", nil)
		return
	}

	response.WriteJSON(w, r, http.StatusOK, toView(row))
}
