// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package sshgrants

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// Get implements GET /v1/ssh-grants/{id}. Required permission:
// vm:ssh-grant. Ownership is checked against the grant's creator; a
// non-owner developer sees 404, shared with the genuinely-absent envelope
// so existence is not leaked. The stored token hash is never surfaced.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	grant, ok := h.loadOwnedGrant(w, r)
	if !ok {
		return
	}
	response.WriteJSON(w, r, http.StatusOK, toView(grant))
}

// loadOwnedGrant resolves the {id} path param, loads the grant, and
// enforces the caller's vm:ssh-grant scope against the grant's creator.
// A bad UUID is 400; a missing grant or a cross-user developer is 404 (no
// existence leak). It returns false and writes the response on any
// failure.
func (h *Handler) loadOwnedGrant(w http.ResponseWriter, r *http.Request) (store.SSHGrant, bool) {
	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
		return store.SSHGrant{}, false
	}

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "id must be a uuid", nil)
		return store.SSHGrant{}, false
	}

	grant, err := h.store.SSHGrantByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeGrantNotFound(w, r)
			return store.SSHGrant{}, false
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load ssh grant", nil)
		return store.SSHGrant{}, false
	}

	owner := grant.CreatedBy
	if err := auth.CheckOwnership(caller, &owner, auth.PermVMSSHGrant); err != nil {
		writeGrantNotFound(w, r)
		return store.SSHGrant{}, false
	}
	return grant, true
}

// writeGrantNotFound emits the shared 404 envelope used both for a
// genuinely-missing grant and for a grant invisible to the caller.
func writeGrantNotFound(w http.ResponseWriter, r *http.Request) {
	response.WriteError(w, r, http.StatusNotFound,
		response.CodeNotFound, "ssh grant not found", nil)
}
