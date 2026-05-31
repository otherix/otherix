// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"context"

	"github.com/google/uuid"
)

// APITokenByID returns the api token with the given id, or ErrNotFound.
func (s *Store) APITokenByID(ctx context.Context, id uuid.UUID) (ApiToken, error) {
	row, err := s.queries.GetApiTokenByID(ctx, id)
	if err != nil {
		return ApiToken{}, translateNoRows(err)
	}
	return row, nil
}

// CreateAPIToken inserts an api token. The caller generates the
// plaintext and its hash; this method persists the row.
func (s *Store) CreateAPIToken(ctx context.Context, arg CreateApiTokenParams) (ApiToken, error) {
	return s.queries.CreateApiToken(ctx, arg)
}

// ListAPITokensByUser returns the cursor-paginated api tokens owned by a
// user. IncludeRevoked toggles whether revoked rows are surfaced.
func (s *Store) ListAPITokensByUser(ctx context.Context, arg ListApiTokensByUserParams) ([]ApiToken, error) {
	return s.queries.ListApiTokensByUser(ctx, arg)
}

// RevokeAPIToken sets revoked_at on the token. The SQL is idempotent
// (no-op when already revoked), so callers can always treat success as
// 204.
func (s *Store) RevokeAPIToken(ctx context.Context, id uuid.UUID) error {
	return s.queries.RevokeApiToken(ctx, id)
}

// APITokenByHash returns the valid (non-revoked, non-expired) api token with the
// given hash, or ErrNotFound. Backs API-token authentication.
func (s *Store) APITokenByHash(ctx context.Context, hash []byte) (ApiToken, error) {
	row, err := s.queries.GetApiTokenByHash(ctx, hash)
	if err != nil {
		return ApiToken{}, translateNoRows(err)
	}
	return row, nil
}

// TouchAPIToken stamps last_used_at on an api token.
func (s *Store) TouchAPIToken(ctx context.Context, id uuid.UUID) error {
	return s.queries.TouchApiToken(ctx, id)
}
