// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"context"

	"github.com/google/uuid"
)

// JoinTokenByID returns the join token with the given id, or ErrNotFound.
func (s *Store) JoinTokenByID(ctx context.Context, id uuid.UUID) (JoinToken, error) {
	row, err := s.queries.GetJoinTokenByID(ctx, id)
	if err != nil {
		return JoinToken{}, translateNoRows(err)
	}
	return row, nil
}

// CreateJoinToken inserts a join token. Callers own all API-edge
// validation (TTL bounds, max_uses, pre-bound single-use); the schema
// CHECK constraints are defense-in-depth.
func (s *Store) CreateJoinToken(ctx context.Context, arg CreateJoinTokenParams) (JoinToken, error) {
	return s.queries.CreateJoinToken(ctx, arg)
}

// ListJoinTokens returns join tokens matching the cursor-paginated
// params. Each row carries a correlated consumption_count.
func (s *Store) ListJoinTokens(ctx context.Context, arg ListJoinTokensParams) ([]ListJoinTokensRow, error) {
	return s.queries.ListJoinTokens(ctx, arg)
}

// RevokeJoinToken expires the join token by clamping expires_at to now.
// The SQL is idempotent (LEAST(expires_at, now())), so re-revoking an
// already-expired token is a no-op.
func (s *Store) RevokeJoinToken(ctx context.Context, id uuid.UUID) error {
	return s.queries.RevokeJoinToken(ctx, id)
}

// ListJoinTokenConsumptions returns the consumption audit rows for a
// token, cursor-paginated.
func (s *Store) ListJoinTokenConsumptions(ctx context.Context, arg ListJoinTokenConsumptionsParams) ([]JoinTokenConsumption, error) {
	return s.queries.ListJoinTokenConsumptions(ctx, arg)
}
