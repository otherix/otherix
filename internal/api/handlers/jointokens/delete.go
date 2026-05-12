// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package jointokens

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
)

// Delete implements DELETE /v1/nodes/join-tokens/{id}. Required
// permission: node:manage. Sets expires_at = LEAST(expires_at, now())
// — а live token jumps к the past, an already-expired token stays
// where it is и the handler returns 409 per the spec.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "id must be a uuid", nil)
		return
	}

	row, err := h.loadToken(r.Context(), id)
	if err != nil {
		if errors.Is(err, errTokenNotFound) {
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "join token not found", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load token", nil)
		return
	}

	if !row.ExpiresAt.After(time.Now()) {
		response.WriteError(w, r, http.StatusConflict,
			response.CodeConflict, "join token already expired", nil)
		return
	}

	if err := h.store.Queries().RevokeJoinToken(r.Context(), id); err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "revoke token", nil)
		return
	}

	caller := auth.UserFromContext(r.Context())
	var callerID string
	if caller != nil {
		callerID = caller.ID.String()
	}
	h.log.InfoContext(r.Context(), "join token revoked",
		slog.String("token_id", id.String()),
		slog.String("revoked_by_user_id", callerID))

	w.WriteHeader(http.StatusNoContent)
}
