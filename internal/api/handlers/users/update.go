// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package users

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/api/validation"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// Update implements PATCH /v1/users/{id}. Required permission:
// user:manage (admin only). Even an admin cannot promote, demote, or
// otherwise change their own `role` — self-promotion is rejected with
// 403 forbidden_fields:["role"]. All other fields (display_name,
// password) are admin-set on any user including themselves.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
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
	var req updateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid request body", nil)
		return
	}

	row, err := h.store.UserByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "user not found", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load user", nil)
		return
	}

	if req.Role != nil && row.ID == caller.ID && *req.Role != row.Role {
		response.WriteError(w, r, http.StatusForbidden,
			response.CodePermissionDenied,
			"caller may not change their own role",
			map[string]any{"forbidden_fields": []string{"role"}})
		return
	}

	roleChanged := req.Role != nil && *req.Role != row.Role

	if !applyMutableFields(w, r, &row, req, true) {
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
	if !h.revokeSessionsIfCredentialsChanged(w, r, req, roleChanged, row.ID) {
		return
	}
	response.WriteJSON(w, r, http.StatusOK, toView(updated))
}

// revokeSessionsIfCredentialsChanged revokes every refresh token of user id
// when the update changed the password OR the role, and is a no-op otherwise.
// Both are credential-equivalent session resets: a password change is the M7
// session-invalidation half (audit M7), and a role change (especially a
// demotion or lockdown) must likewise drop the user's live refresh sessions so
// a stolen or stale refresh token cannot keep minting tokens after the operator
// changed the user's standing (audit R2-L1). Access JWTs are stateless and
// survive up to their short TTL by design. A revoke failure is surfaced as 500
// rather than swallowed, because silently leaving sessions alive defeats the
// point; it returns false after writing that error and true otherwise.
func (h *Handler) revokeSessionsIfCredentialsChanged(w http.ResponseWriter, r *http.Request, req updateRequest, roleChanged bool, id uuid.UUID) bool {
	if req.Password == nil && !roleChanged {
		return true
	}
	if err := h.store.RevokeAllUserRefreshTokens(r.Context(), id); err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "revoke sessions", nil)
		return false
	}
	return true
}

// applyMutableFields merges the optional fields of req into row in
// place. It returns false after writing an error response when a field
// is invalid, in which case the caller must abort. The allowRole flag
// gates the role merge: PATCH /me passes false (the role-rejection
// happens earlier so the user gets a clearer error message); PATCH
// /{id} passes true because the self-role guard above already filtered
// the disallowed case.
func applyMutableFields(w http.ResponseWriter, r *http.Request, row *store.User, req updateRequest, allowRole bool) bool {
	if allowRole && req.Role != nil {
		newRole, err := auth.ParseRole(*req.Role)
		if err != nil {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, "invalid role", nil)
			return false
		}
		row.Role = string(newRole)
	}
	if req.DisplayName != nil {
		row.DisplayName = *req.DisplayName
	}
	if req.Password != nil {
		if err := validation.ValidatePassword(*req.Password); err != nil {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, err.Error(), nil)
			return false
		}
		hash, err := auth.HashPassword(*req.Password)
		if err != nil {
			response.WriteError(w, r, http.StatusInternalServerError,
				response.CodeInternal, "hash password", nil)
			return false
		}
		row.PasswordHash = hash
	}
	return true
}
