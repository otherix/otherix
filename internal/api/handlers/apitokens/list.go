// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package apitokens

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
)

// ListForUser implements GET /v1/users/{id}/api-tokens. Required
// permission: api_token:manage (admin: any; others: own with caller-id
// equal to {id}; otherwise 404). Honours `?include_revoked` (default
// false). Cursor pagination.
func (h *Handler) ListForUser(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
		return
	}

	targetID, err := h.resolveTargetUser(r.Context(), chi.URLParam(r, "id"), caller)
	if err != nil {
		switch {
		case errors.Is(err, errBadTargetID):
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, "id must be a uuid", nil)
		case errors.Is(err, errTargetNotFound):
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "user not found", nil)
		default:
			response.WriteError(w, r, http.StatusInternalServerError,
				response.CodeInternal, "load user", nil)
		}
		return
	}

	out, err := h.listForUser(r.Context(), targetID, parseIncludeRevoked(r), r)
	if err != nil {
		if errors.Is(err, errInvalidCursor) {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, "invalid cursor", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "list tokens", nil)
		return
	}

	response.WriteJSON(w, r, http.StatusOK, out)
}
