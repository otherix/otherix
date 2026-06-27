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
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/api/validation"
	coreauth "github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/ratelimit"
	"github.com/otherix/otherix/internal/store"
)

// Store is the storage surface the auth handlers depend on: a single
// user-by-username lookup that Login uses to surface the authenticated
// user inline in LoginResponse. *etcdstore.Store satisfies it; depending on
// the interface rather than the concrete store narrows the handler's storage
// dependency to the methods it uses and lets tests substitute a fake.
type Store interface {
	UserByUsername(ctx context.Context, username string) (store.User, error)
}

// Ensure the production store satisfies the handler's storage contract.

// Service is the auth-service surface the handlers depend on.
// *coreauth.Service satisfies it; depending on the interface narrows the
// handler's dependency to the methods it calls and lets tests substitute
// a spy (e.g. to assert the rate limiter short-circuits Login before the
// argon2 verification path runs).
type Service interface {
	Login(ctx context.Context, creds coreauth.Credentials) (*coreauth.TokenPair, error)
	Refresh(ctx context.Context, plaintext, userAgent string, ip netip.Addr) (*coreauth.TokenPair, error)
	Logout(ctx context.Context, plaintext string) error
	LogoutAll(ctx context.Context, userID uuid.UUID) error
}

// Ensure the production auth service satisfies the handler's contract.
var _ Service = (*coreauth.Service)(nil)

// Handler bundles dependencies for the /v1/auth/* routes.
type Handler struct {
	svc   Service
	store Store
	// loginLimiter throttles repeated FAILED logins per source IP and
	// per target email before the argon2id verify runs. Nil disables
	// throttling entirely (fail-open): login behaves exactly as it did
	// before the limiter existed.
	loginLimiter *ratelimit.FailureLimiter
}

// New constructs a Handler. The service and store are required; the user
// store is needed by Login to surface the authenticated user inline in
// LoginResponse per the OpenAPI shape. loginLimiter may be nil, which
// disables login-failure throttling.
func New(svc Service, s Store, loginLimiter *ratelimit.FailureLimiter) *Handler {
	return &Handler{svc: svc, store: s, loginLimiter: loginLimiter}
}

type loginRequest struct {
	Username string `json:"username"`
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
	Username    string  `json:"username"`
	Email       string  `json:"email,omitempty"`
	DisplayName string  `json:"display_name"`
	Role        string  `json:"role"`
	LastLoginAt *string `json:"last_login_at"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
}

func toUserView(u store.User) userView {
	v := userView{
		ID:          u.ID.String(),
		Username:    u.Username,
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

// Login authenticates by username + password and returns access+refresh tokens.
//
// Before the credential check (an argon2id verify costing ~19 MiB and
// tens of milliseconds per call) the handler consults the optional
// failed-login rate limiter under two keys: the source IP and the
// lowercased target username. Either key being over budget short-circuits
// to 429 rate_limited without touching argon2, capping brute-force CPU
// and memory amplification. Only FAILED credential checks are recorded:
// successful logins never count toward or reset the window, so a success
// can neither extend nor clear an existing block. Note this means an
// in-window block still 429s any attempt on that key, INCLUDING one with
// a correct password, until the window passes; a success simply does not
// record, it does not unblock. The worst false-positive is a throttled
// legitimate user who retries after the window. That lowers net risk (a
// recoverable throttle replaces a recoverable DoS) with no irreversible
// failure mode.
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid request body", nil)
		return
	}
	if req.Username == "" || req.Password == "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "username and password are required", nil)
		return
	}
	// Reject an over-long username before building limiter keys or running
	// argon2. A >UsernameMaxLength username can never match a stored user
	// (every stored username passed validation, <= UsernameMaxLength), so
	// this only short-circuits input that would have 401'd anyway after a
	// wasted dummy KDF - no legitimate login changes. This also caps the
	// per-username limiter key at <= UsernameMaxLength bytes, so
	// attacker-controlled username bytes cannot turn the bounded limiter
	// into a memory vector. The login path deliberately does NOT run the
	// full username syntax validation: rejecting a syntactically-invalid
	// username with 400 (while an unknown-but-valid username falls through
	// to the dummy-hash 401 path) would expose a status/timing oracle the
	// anti-enumeration dummy hash cannot mask. Empty-check and length-clamp
	// only.
	if len(req.Username) > validation.UsernameMaxLength {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "username is too long", nil)
		return
	}

	addr := clientAddr(r)
	limiterKeys := loginLimiterKeys(addr, req.Username)
	if h.rejectIfLoginThrottled(w, r, limiterKeys) {
		return
	}

	pair, err := h.svc.Login(r.Context(), coreauth.Credentials{
		Username:  req.Username,
		Password:  req.Password,
		UserAgent: r.UserAgent(),
		IP:        addr,
	})
	if err != nil {
		if errors.Is(err, coreauth.ErrInvalidCredentials) {
			// Count only credential failures: successes are never
			// recorded, and failures age out of the window on their
			// own (no clear-on-success, so a slow brute force cannot
			// reset its budget by sprinkling in valid logins).
			if h.loginLimiter != nil {
				for _, k := range limiterKeys {
					h.loginLimiter.RecordFailure(k)
				}
			}
			response.WriteError(w, r, http.StatusUnauthorized,
				response.CodeInvalidCredentials, "invalid username or password", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "login failed", nil)
		return
	}

	user, err := h.store.UserByUsername(r.Context(), req.Username)
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

// loginLimiterKeys builds the failure-counter keys for one login
// attempt: the source IP and the lowercased target username. The ip key
// is skipped when addr is the zero netip.Addr so unattributable requests
// do not collapse into one shared bucket; the username key still
// constrains attacks on a single account. Behind a reverse proxy the
// per-IP key collapses to the proxy address, so the per-username key
// carries the weight there too (accepted tradeoff).
func loginLimiterKeys(addr netip.Addr, username string) []string {
	var keys []string
	if addr.IsValid() {
		keys = append(keys, "ip:"+addr.String())
	}
	return append(keys, "username:"+strings.ToLower(username))
}

// rejectIfLoginThrottled writes a 429 rate_limited response and reports
// true when any key is over the failed-login budget. Retry-After (and
// details.retry_after_seconds) is the ceiling in seconds of the LARGER
// remaining block among the keys, clamped to at least 1. A nil limiter
// never throttles.
func (h *Handler) rejectIfLoginThrottled(w http.ResponseWriter, r *http.Request, keys []string) bool {
	if h.loginLimiter == nil {
		return false
	}
	var blocked bool
	var retryAfter time.Duration
	for _, k := range keys {
		if b, ra := h.loginLimiter.Blocked(k); b {
			blocked = true
			if ra > retryAfter {
				retryAfter = ra
			}
		}
	}
	if !blocked {
		return false
	}
	secs := int64((retryAfter + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
	response.WriteError(w, r, http.StatusTooManyRequests,
		response.CodeRateLimited, "too many failed login attempts, retry later",
		map[string]any{"retry_after_seconds": secs})
	return true
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
