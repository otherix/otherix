// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package sshgrants

import (
	"errors"
	"net/http"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/store"
)

// Revoke implements POST /v1/ssh-grants/{id}/revoke. Required permission:
// vm:ssh-grant. The caller must own the grant (else 404). Revoke flags the
// grant and keeps the row (and its token index) so a connect attempt is
// rejected uniformly rather than leaking revocation as not-found.
// Idempotent: revoking an already-revoked grant succeeds. Returns 200 with
// the updated grant.
func (h *Handler) Revoke(w http.ResponseWriter, r *http.Request) {
	grant, ok := h.loadOwnedGrant(w, r)
	if !ok {
		return
	}

	if err := h.store.RevokeSSHGrant(r.Context(), grant.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeGrantNotFound(w, r)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "revoke ssh grant", nil)
		return
	}

	updated, err := h.store.SSHGrantByID(r.Context(), grant.ID)
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load ssh grant", nil)
		return
	}
	response.WriteJSON(w, r, http.StatusOK, toView(updated))
}
