// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package auth wires the auth.Service into the HTTP surface: login,
// refresh, and logout endpoints. The handler shapes match the
// LoginRequest / LoginResponse / RefreshRequest / RefreshResponse /
// LogoutRequest schemas in api/openapi/control-plane.yaml exactly.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/otherix/otherix/internal/api/response"
	coreauth "github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// Store is the storage surface the auth handlers depend on: a single
// user-by-email lookup that Login uses to surface the authenticated
// user inline in LoginResponse. *etcdstore.Store satisfies it; depending on
// the interface rather than the concrete store narrows the handler's storage
// dependency to the methods it uses and lets tests substitute a fake.
type Store interface {
	UserByEmail(ctx context.Context, email string) (store.User, error)
}

// Ensure the production store satisfies the handler's storage contract.

// Handler bundles dependencies for the /v1/auth/* routes.
type Handler struct {
	svc   *coreauth.Service
	store Store
}

// New constructs a Handler. Both dependencies are required; the user
// store is needed by Login to surface the authenticated user inline in
// LoginResponse per the OpenAPI shape.
func New(svc *coreauth.Service, s Store) *Handler {
	return &Handler{svc: svc, store: s}
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type loginResponse struct {
	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token"`
	TokenType    string   `json:"token_type"`
	ExpiresIn    int      `json:"expires_in"`
	User         userView `json:"user"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type refreshResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
}

type logoutRequest struct {
	RefreshToken *string `json:"refresh_token"`
	AllSessions  bool    `json:"all_sessions"`
}

// userView mirrors the OpenAPI User schema. Defined here rather than in
// a shared package because Login is the only auth handler that returns
// it; the users handler in internal/api/handlers/users keeps its own
// copy. Both must move in lockstep with the spec — a future refactor
// can extract a shared package once a third surface needs it.
type userView struct {
	ID          string  `json:"id"`
	Email       string  `json:"email"`
	DisplayName string  `json:"display_name"`
	Role        string  `json:"role"`
	LastLoginAt *string `json:"last_login_at"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
}

func toUserView(u store.User) userView {
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

// Login authenticates by email + password and returns access+refresh tokens.
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid request body", nil)
		return
	}
	if req.Email == "" || req.Password == "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "email and password are required", nil)
		return
	}

	pair, err := h.svc.Login(r.Context(), coreauth.Credentials{
		Email:     req.Email,
		Password:  req.Password,
		UserAgent: r.UserAgent(),
		IP:        clientAddr(r),
	})
	if err != nil {
		if errors.Is(err, coreauth.ErrInvalidCredentials) {
			response.WriteError(w, r, http.StatusUnauthorized,
				response.CodeInvalidCredentials, "invalid email or password", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "login failed", nil)
		return
	}

	user, err := h.store.UserByEmail(r.Context(), req.Email)
	if err != nil {
		// Login just succeeded; the user must exist. A failure here
		// is genuinely a server problem.
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load user", nil)
		return
	}

	response.WriteJSON(w, r, http.StatusOK, loginResponse{
		AccessToken:  pair.AccessToken,
		RefreshToken: pair.RefreshToken,
		TokenType:    "Bearer",
		ExpiresIn:    pair.AccessExpiresIn,
		User:         toUserView(user),
	})
}

// Refresh rotates the refresh token and issues a fresh pair.
func (h *Handler) Refresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RefreshToken == "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "refresh_token is required", nil)
		return
	}

	pair, err := h.svc.Refresh(r.Context(), req.RefreshToken, r.UserAgent(), clientAddr(r))
	if err != nil {
		switch {
		case errors.Is(err, coreauth.ErrTokenExpired),
			errors.Is(err, coreauth.ErrInvalidToken),
			errors.Is(err, coreauth.ErrTokenReplay):
			response.WriteError(w, r, http.StatusUnauthorized,
				response.CodeUnauthenticated, "refresh token rejected", nil)
		default:
			response.WriteError(w, r, http.StatusInternalServerError,
				response.CodeInternal, "refresh failed", nil)
		}
		return
	}

	response.WriteJSON(w, r, http.StatusOK, refreshResponse{
		AccessToken:  pair.AccessToken,
		RefreshToken: pair.RefreshToken,
		TokenType:    "Bearer",
		ExpiresIn:    pair.AccessExpiresIn,
	})
}

// Logout revokes the supplied refresh token, or every active token of
// the calling user if all_sessions is true. Always returns 204 on a
// successful path; an absent / unknown refresh token is not an error.
//
// Authn middleware must run before this handler so r.Context() carries
// an *auth.User. The all_sessions branch needs the user id; the
// targeted branch ignores the principal and acts solely on the token.
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	var req logoutRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, "invalid request body", nil)
			return
		}
	}

	if req.AllSessions {
		caller := coreauth.UserFromContext(r.Context())
		if caller == nil {
			response.WriteError(w, r, http.StatusUnauthorized,
				response.CodeUnauthenticated, "missing principal", nil)
			return
		}
		// LogoutAll revokes via an idempotent :exec, so a user with no
		// active sessions is a successful no-op rather than a missing-row
		// error; any error here is a genuine database fault.
		if err := h.svc.LogoutAll(r.Context(), caller.ID); err != nil {
			response.WriteError(w, r, http.StatusInternalServerError,
				response.CodeInternal, "logout failed", nil)
			return
		}
		response.WriteNoContent(w)
		return
	}

	if req.RefreshToken == nil || *req.RefreshToken == "" {
		// No refresh_token and not all_sessions is a no-op per spec
		// (the OpenAPI summary phrases logout as idempotent revocation).
		response.WriteNoContent(w)
		return
	}

	if err := h.svc.Logout(r.Context(), *req.RefreshToken); err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "logout failed", nil)
		return
	}
	response.WriteNoContent(w)
}

// clientAddr extracts the request's source IP. RemoteAddr is "host:port";
// we strip the port and try to parse. Returns the zero netip.Addr on
// any failure — the auth.Service treats invalid as "no IP recorded".
func clientAddr(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return addr
}
