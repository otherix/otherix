// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package users hosts the /v1/users/* HTTP handlers. The full CRUD
// surface (create, list, get, update, update-me, delete) is gated by
// `user:read` and `user:manage` per docs/rbac.md; the self-service
// /v1/users/me endpoints are reachable for any authenticated principal.
package users

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// Store is the storage surface the users handlers depend on. Depending
// on the interface rather than the concrete *etcdstore.Store narrows the
// handler's storage dependency to the methods it uses and lets tests
// substitute a fake. *etcdstore.Store satisfies it.
type Store interface {
	UserByID(ctx context.Context, id uuid.UUID) (store.User, error)
	UserByEmail(ctx context.Context, email string) (store.User, error)
	CreateUser(ctx context.Context, arg store.CreateUserParams) (store.User, error)
	UpdateUser(ctx context.Context, arg store.UpdateUserParams) (store.User, error)
	ListUsers(ctx context.Context, arg store.ListUsersParams) ([]store.User, error)
	CountUserResources(ctx context.Context, id uuid.UUID) (store.CountUserResourcesRow, error)
	DeleteUser(ctx context.Context, id uuid.UUID) error
}

// Ensure the production store satisfies the handler's storage contract.

// Handler bundles the data-access dependencies for the users routes.
type Handler struct {
	store Store
}

// New constructs a Handler. It takes the Store interface so any
// conforming backend can be wired in; production passes *store.Store.
func New(s Store) *Handler {
	return &Handler{store: s}
}

// userView is the public projection of a users row. It deliberately
// omits password_hash so the helper cannot accidentally surface it.
type userView struct {
	ID          string  `json:"id"`
	Email       string  `json:"email"`
	DisplayName string  `json:"display_name"`
	Role        string  `json:"role"`
	LastLoginAt *string `json:"last_login_at"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
}

func toView(u store.User) userView {
	v := userView{
		ID:          u.ID.String(),
		Email:       u.Email,
		DisplayName: u.DisplayName,
		Role:        u.Role,
		CreatedAt:   u.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:   u.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	if u.LastLoginAt != nil {
		s := u.LastLoginAt.UTC().Format(time.RFC3339Nano)
		v.LastLoginAt = &s
	}
	return v
}

// GetMe returns the calling user. Requires the Authn middleware to
// have populated the principal in the request context.
func (h *Handler) GetMe(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
		return
	}

	u, err := h.store.UserByID(r.Context(), caller.ID)
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

	response.WriteJSON(w, r, http.StatusOK, toView(u))
}
