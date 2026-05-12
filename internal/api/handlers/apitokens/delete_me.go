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

// DeleteMe revokes one of the calling user's tokens. 404 if the token
// is unknown OR belongs to another user — both leak existence the same
// way. Already-revoked tokens emit 204 (RevokeApiToken is idempotent
// at the SQL layer).
func (h *Handler) DeleteMe(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
		return
	}

	tokenID, err := uuid.Parse(chi.URLParam(r, "token_id"))
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "token_id must be a uuid", nil)
		return
	}

	if _, err := h.loadAndScopeToken(r.Context(), tokenID, caller.ID); err != nil {
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
