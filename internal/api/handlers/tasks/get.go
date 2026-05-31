// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package tasks

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// Get implements GET /v1/tasks/{id}. Required permission: `task:read`.
//
// Visibility rules:
//   - admin / operator (ScopeAny) → 200 with the row.
//   - developer / viewer (ScopeOwn) → 200 only when the row's
//     created_by equals the caller; otherwise 404 (NOT 403, per
//     CLAUDE.md no-existence-leak rule). NULL created_by — possible
//     when the originating user was soft-deleted with ON DELETE SET
//     NULL — is also invisible to scope=own callers.
//
// Genuinely missing rows return 404 too; the two paths share the same
// envelope so callers cannot distinguish "exists but invisible" from
// "does not exist".
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
		return
	}

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "id must be a uuid", nil)
		return
	}

	row, err := h.store.TaskByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeTaskNotFound(w, r)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load task", nil)
		return
	}

	switch auth.ScopeFor(caller.Role, auth.PermTaskRead) {
	case auth.ScopeAny:
		// Visible.
	case auth.ScopeOwn:
		if row.CreatedBy == nil || *row.CreatedBy != caller.ID {
			writeTaskNotFound(w, r)
			return
		}
	default:
		// Defensive: middleware should have rejected. 404 is the
		// no-leak fall-through.
		writeTaskNotFound(w, r)
		return
	}

	response.WriteJSON(w, r, http.StatusOK, toView(row))
}

func writeTaskNotFound(w http.ResponseWriter, r *http.Request) {
	response.WriteError(w, r, http.StatusNotFound,
		response.CodeNotFound, "task not found", nil)
}
