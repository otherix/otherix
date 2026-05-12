// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package users

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/api/validation"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// Create implements POST /v1/users. Required permission: user:manage
// (admin only by docs/rbac.md). The plaintext password is hashed via
// argon2id before insertion and never echoed in the response.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid request body", nil)
		return
	}

	if err := validation.ValidateEmail(req.Email); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, err.Error(), nil)
		return
	}
	if err := validation.ValidatePassword(req.Password); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, err.Error(), nil)
		return
	}
	role, err := auth.ParseRole(req.Role)
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid role", nil)
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "hash password", nil)
		return
	}

	row, err := h.store.Queries().CreateUser(r.Context(), store.CreateUserParams{
		ID:           uuid.New(),
		Email:        req.Email,
		PasswordHash: hash,
		DisplayName:  req.DisplayName,
		Role:         string(role),
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			response.WriteError(w, r, http.StatusConflict,
				response.CodeConflict, "email already in use", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "persist user", nil)
		return
	}

	response.WriteJSON(w, r, http.StatusCreated, toView(row))
}
