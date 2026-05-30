// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package apitokens hosts the API-token HTTP handlers. Two parallel
// surfaces share the same data-access core:
//
//   - Self-service (/v1/users/me/api-tokens*) — any authenticated
//     principal may manage their own tokens.
//   - Admin-on-behalf (/v1/users/{id}/api-tokens*) — admin scope=any,
//     other roles scope=own; a non-admin targeting another user's id
//     gets 404 (no existence leak) via auth.CheckOwnership.
//
// Both surfaces honour the spec's `?include_revoked` query parameter
// (default false). The /me handlers are thin adapters over the same
// shared core the /{id} handlers use.
package apitokens

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/pagination"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// Store is the storage surface the api-tokens handlers depend on: the
// api-token domain methods plus the target-user existence check used by
// the admin-on-behalf surface. *store.Store satisfies it; depending on
// the interface rather than the concrete store is the Phase 2 seam that
// lets a second backend (Phase 3) be substituted under the same handler
// tests.
type Store interface {
	UserByID(ctx context.Context, id uuid.UUID) (store.User, error)
	APITokenByID(ctx context.Context, id uuid.UUID) (store.ApiToken, error)
	CreateAPIToken(ctx context.Context, arg store.CreateApiTokenParams) (store.ApiToken, error)
	ListAPITokensByUser(ctx context.Context, arg store.ListApiTokensByUserParams) ([]store.ApiToken, error)
	RevokeAPIToken(ctx context.Context, id uuid.UUID) error
}

// Ensure the production store satisfies the handler's storage contract.
var _ Store = (*store.Store)(nil)

// Handler holds the dependencies of the api-tokens routes.
type Handler struct {
	store Store
}

// New constructs a Handler.
func New(s Store) *Handler {
	return &Handler{store: s}
}

// errBadTargetID signals a non-uuid {id} URL parameter; the caller
// maps it to 400.
var errBadTargetID = errors.New("invalid target user id")

// errTargetNotFound signals "target user is missing or invisible to
// caller" — covers both store.ErrNotFound and ownership mismatch on
// scope=own. Both surface as 404 to avoid existence leak.
var errTargetNotFound = errors.New("target user not found")

// errInvalidExpiry signals an expires_at that does not parse or sits in
// the past. The caller maps it to 400 validation_failed.
var errInvalidExpiry = errors.New("invalid expires_at")

// errEmptyName signals a missing or blank name field.
var errEmptyName = errors.New("name is required")

// errNameTooLong signals a name longer than the spec maximum.
var errNameTooLong = errors.New("name exceeds 100 characters")

// nameMaxLength mirrors ApiTokenCreate.name.maxLength in the OpenAPI spec.
const nameMaxLength = 100

// parseCreateRequest decodes the JSON body and validates name and
// expires_at. Returns a sentinel error suitable for direct status
// mapping by the caller.
func parseCreateRequest(r *http.Request) (createRequest, *time.Time, error) {
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return req, nil, err
	}
	if req.Name == "" {
		return req, nil, errEmptyName
	}
	if len(req.Name) > nameMaxLength {
		return req, nil, errNameTooLong
	}

	var expiresAt *time.Time
	if req.ExpiresAt != nil && *req.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339Nano, *req.ExpiresAt)
		if err != nil {
			return req, nil, errInvalidExpiry
		}
		if !t.After(time.Now()) {
			return req, nil, errInvalidExpiry
		}
		expiresAt = &t
	}
	return req, expiresAt, nil
}

// createForUser is the shared body of CreateMe and CreateForUser. It
// generates a fresh token, persists it owned by userID, and returns the
// stored row plus the plaintext (returned to the client exactly once).
func (h *Handler) createForUser(ctx context.Context, userID uuid.UUID, req createRequest, expiresAt *time.Time) (store.ApiToken, string, error) {
	plaintext, hash, prefix, err := auth.GenerateAPIToken()
	if err != nil {
		return store.ApiToken{}, "", err
	}
	row, err := h.store.CreateAPIToken(ctx, store.CreateApiTokenParams{
		ID:        uuid.New(),
		UserID:    userID,
		Name:      req.Name,
		TokenHash: hash,
		Prefix:    prefix,
		ExpiresAt: expiresAt,
	})
	if err != nil {
		return store.ApiToken{}, "", err
	}
	return row, plaintext, nil
}

// listForUser is the shared body of ListMe and ListForUser.
// includeRevoked toggles whether revoked rows are surfaced; cursor
// pagination.
func (h *Handler) listForUser(ctx context.Context, userID uuid.UUID, includeRevoked bool, raw *http.Request) (listResponse, error) {
	limit := pagination.Limit(pagination.ParseLimit(raw.URL.Query().Get("limit")))

	cur, err := pagination.Decode(raw.URL.Query().Get("cursor"))
	if err != nil {
		return listResponse{}, errInvalidCursor
	}

	params := store.ListApiTokensByUserParams{
		UserID:         userID,
		IncludeRevoked: includeRevoked,
		LimitCount:     limit + 1, // +1 row to detect "is there a next page".
	}
	if cur != nil {
		params.CursorCreatedAt = &cur.CreatedAt
		params.CursorID = &cur.ID
	}

	rows, err := h.store.ListAPITokensByUser(ctx, params)
	if err != nil {
		return listResponse{}, err
	}

	limitN := int(limit)
	var nextCursor *string
	if len(rows) > limitN {
		last := rows[limitN-1]
		s := pagination.Encode(&pagination.Cursor{CreatedAt: last.CreatedAt, ID: last.ID})
		nextCursor = &s
		rows = rows[:limitN]
	}

	views := make([]tokenView, 0, len(rows))
	for _, t := range rows {
		views = append(views, toView(t))
	}
	return listResponse{
		Data: views,
		Meta: paginationMeta{NextCursor: nextCursor},
	}, nil
}

// errInvalidCursor is the sentinel for a malformed pagination cursor;
// callers map it to 400 validation_failed.
var errInvalidCursor = errors.New("invalid cursor")

// errTokenNotFound is the sentinel for a token row missing or
// out-of-scope. Callers map it to 404.
var errTokenNotFound = errors.New("api token not found")

// loadAndScopeToken fetches the token row and verifies it belongs to
// expectedOwner, returning errTokenNotFound on either lookup miss or
// owner mismatch. URI mismatch ({id} ≠ token.user_id) is surfaced as
// 404 even for admin — the path describes a token that does not exist
// at that location.
func (h *Handler) loadAndScopeToken(ctx context.Context, tokenID, expectedOwner uuid.UUID) (store.ApiToken, error) {
	row, err := h.store.APITokenByID(ctx, tokenID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.ApiToken{}, errTokenNotFound
		}
		return store.ApiToken{}, err
	}
	if row.UserID != expectedOwner {
		return store.ApiToken{}, errTokenNotFound
	}
	return row, nil
}

// revokeToken issues the underlying RevokeApiToken update. The SQL is
// idempotent (no-op when revoked_at is already set), so callers can
// always emit 204 on success.
func (h *Handler) revokeToken(ctx context.Context, tokenID uuid.UUID) error {
	return h.store.RevokeAPIToken(ctx, tokenID)
}

// resolveTargetUser parses the {id} URL parameter, loads the target
// user, and verifies the caller may act on tokens owned by them.
// Returns errBadTargetID for a malformed uuid and errTargetNotFound
// when either the user is missing/soft-deleted or the caller's scope
// (own/any) does not cover the target. Both lookup miss and scope
// mismatch collapse to 404 in the handler — never leaking which user
// ids exist via differing status codes.
func (h *Handler) resolveTargetUser(ctx context.Context, raw string, caller *auth.User) (uuid.UUID, error) {
	targetID, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, errBadTargetID
	}
	if _, err := h.store.UserByID(ctx, targetID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return uuid.Nil, errTargetNotFound
		}
		return uuid.Nil, err
	}
	if err := auth.CheckOwnership(caller, &targetID, auth.PermAPITokenManage); err != nil {
		if errors.Is(err, auth.ErrPermissionDenied) {
			return uuid.Nil, errTargetNotFound
		}
		return uuid.Nil, err
	}
	return targetID, nil
}

// parseIncludeRevoked reads the `?include_revoked` query parameter.
// Default is false (matches the spec); only the literal string "true"
// flips it on.
func parseIncludeRevoked(r *http.Request) bool {
	return r.URL.Query().Get("include_revoked") == "true"
}

// toView projects a store.ApiToken into the public tokenView. The
// plaintext token and the storage hash are NEVER part of the view.
func toView(t store.ApiToken) tokenView {
	v := tokenView{
		ID:        t.ID.String(),
		UserID:    t.UserID.String(),
		Name:      t.Name,
		Prefix:    t.Prefix,
		CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if t.LastUsedAt != nil {
		s := t.LastUsedAt.UTC().Format(time.RFC3339Nano)
		v.LastUsedAt = &s
	}
	if t.ExpiresAt != nil {
		s := t.ExpiresAt.UTC().Format(time.RFC3339Nano)
		v.ExpiresAt = &s
	}
	if t.RevokedAt != nil {
		s := t.RevokedAt.UTC().Format(time.RFC3339Nano)
		v.RevokedAt = &s
	}
	return v
}

// writeCreateError maps the validation sentinels from parseCreateRequest
// to 400 envelopes; anything else is treated as a malformed body.
func writeCreateError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errEmptyName):
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "name is required", nil)
	case errors.Is(err, errNameTooLong):
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "name must be at most 100 characters", nil)
	case errors.Is(err, errInvalidExpiry):
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "expires_at must be RFC3339 in the future", nil)
	default:
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid request body", nil)
	}
}
