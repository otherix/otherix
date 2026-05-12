// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package apitokens

import (
	"errors"
	"net/http"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
)

// ListMe returns the calling user's API tokens with cursor pagination.
// Honours the spec's `?include_revoked` query parameter (default false);
// expired-but-not-revoked tokens are always surfaced so the UI can
// explain why a token stopped working.
func (h *Handler) ListMe(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
		return
	}

	out, err := h.listForUser(r.Context(), caller.ID, parseIncludeRevoked(r), r)
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
