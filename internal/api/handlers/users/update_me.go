// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package users

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// UpdateMe implements PATCH /v1/users/me. Available to any
// authenticated principal — no role gate. The caller may change their
// own display_name and password; including `role` in the payload is
// rejected with 403 forbidden_fields:["role"], regardless of caller's
// role (admin self-role-change goes through PATCH /v1/users/{id}, and
// even there is rejected — only an admin acting on a different user
// may set role).
func (h *Handler) UpdateMe(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
		return
	}

	var req updateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid request body", nil)
		return
	}
	if req.Role != nil {
		response.WriteError(w, r, http.StatusForbidden,
			response.CodePermissionDenied,
			"role may only be changed by an admin via /v1/users/{id}",
			map[string]any{"forbidden_fields": []string{"role"}})
		return
	}

	row, err := h.store.UserByID(r.Context(), caller.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			response.WriteError(w, r, http.StatusUnauthorized,
				response.CodeUnauthenticated, "user no longer exists", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load user", nil)
		return
	}

	if !applyMutableFields(w, r, &row, req, false) {
		return
	}

	updated, err := h.store.UpdateUser(r.Context(), store.UpdateUserParams{
		ID:           row.ID,
		Email:        row.Email,
		PasswordHash: row.PasswordHash,
		DisplayName:  row.DisplayName,
		Role:         row.Role,
	})
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "update user", nil)
		return
	}
	if !h.revokeSessionsIfPasswordChanged(w, r, req, row.ID) {
		return
	}
	response.WriteJSON(w, r, http.StatusOK, toView(updated))
}
