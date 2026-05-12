// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package apitokens

import (
	"net/http"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
)

// CreateMe issues a new API token for the calling user. Plaintext is
// returned exactly once in the response body — the server stores only
// sha256(token).
func (h *Handler) CreateMe(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
		return
	}

	req, expiresAt, err := parseCreateRequest(r)
	if err != nil {
		writeCreateError(w, r, err)
		return
	}

	row, plaintext, err := h.createForUser(r.Context(), caller.ID, req, expiresAt)
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
