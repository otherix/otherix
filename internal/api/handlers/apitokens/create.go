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

// CreateForUser implements POST /v1/users/{id}/api-tokens. Required
// permission: api_token:manage. Admin holds scope=any so it may target
// any user; non-admin roles are scope=own and the caller targeting
// another user's id sees 404 (no existence leak). Plaintext is
// returned exactly once.
func (h *Handler) CreateForUser(w http.ResponseWriter, r *http.Request) {
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

	req, expiresAt, err := parseCreateRequest(r)
	if err != nil {
		writeCreateError(w, r, err)
		return
	}

	row, plaintext, err := h.createForUser(r.Context(), targetID, req, expiresAt)
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "create token", nil)
		return
	}

	response.WriteJSON(w, r, http.StatusCreated, createResponse{
		tokenView: toView(row),
		Token:     plaintext,
	})
}
