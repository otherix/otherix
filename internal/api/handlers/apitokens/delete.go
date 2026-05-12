// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package apitokens

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
)

// DeleteForUser implements DELETE /v1/users/{id}/api-tokens/{token_id}.
// Required permission: api_token:manage (admin: any; others: own).
//
// 404 returns cover three indistinguishable cases — never leaking
// existence via differing status codes:
//
//   - {id} does not match a real user (or is invisible to caller).
//   - {token_id} does not match a real token.
//   - {token_id} exists but does not belong to {id} (URI mismatch).
//
// Already-revoked tokens emit 204; RevokeApiToken is idempotent at
// the SQL layer.
func (h *Handler) DeleteForUser(w http.ResponseWriter, r *http.Request) {
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
				response.CodeNotFound, "api token not found", nil)
		default:
			response.WriteError(w, r, http.StatusInternalServerError,
				response.CodeInternal, "load user", nil)
		}
		return
	}

	tokenID, err := uuid.Parse(chi.URLParam(r, "token_id"))
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "token_id must be a uuid", nil)
		return
	}

	if _, err := h.loadAndScopeToken(r.Context(), tokenID, targetID); err != nil {
		if errors.Is(err, errTokenNotFound) {
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "api token not found", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load token", nil)
		return
	}

	if err := h.revokeToken(r.Context(), tokenID); err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "revoke token", nil)
		return
	}
	response.WriteNoContent(w)
}
