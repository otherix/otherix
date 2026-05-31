// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Refresh-token wrappers expose the rotating-token surface auth.Service needs as
// direct methods (translating pgx.ErrNoRows to ErrNotFound), so the service can
// depend on an interface both *store.Store and *etcdstore.Store satisfy.

// CreateRefreshToken persists a refresh-token row.
func (s *Store) CreateRefreshToken(ctx context.Context, arg CreateRefreshTokenParams) (RefreshToken, error) {
	return s.queries.CreateRefreshToken(ctx, arg)
}

// RefreshTokenByHash returns the row with the given token hash regardless of
// revocation state, or ErrNotFound.
func (s *Store) RefreshTokenByHash(ctx context.Context, hash []byte) (RefreshToken, error) {
	row, err := s.queries.GetRefreshTokenByHash(ctx, hash)
	if err != nil {
		return RefreshToken{}, translateNoRows(err)
	}
	return row, nil
}

// RevokeRefreshToken stamps revoked_at on a single token (idempotent).
func (s *Store) RevokeRefreshToken(ctx context.Context, id uuid.UUID) error {
	return s.queries.RevokeRefreshToken(ctx, id)
}

// RevokeRefreshTokenFamily revokes every active token in the family (the
// theft-detection cascade).
func (s *Store) RevokeRefreshTokenFamily(ctx context.Context, familyID uuid.UUID) error {
	return s.queries.RevokeRefreshTokenFamily(ctx, familyID)
}

// RevokeAllUserRefreshTokens revokes every active token for the user.
func (s *Store) RevokeAllUserRefreshTokens(ctx context.Context, userID uuid.UUID) error {
	return s.queries.RevokeAllUserRefreshTokens(ctx, userID)
}

// TouchRefreshToken stamps last_used_at.
func (s *Store) TouchRefreshToken(ctx context.Context, id uuid.UUID) error {
	return s.queries.TouchRefreshToken(ctx, id)
}

// DeleteExpiredRefreshTokens removes tokens past cutoff, returning the count.
func (s *Store) DeleteExpiredRefreshTokens(ctx context.Context, cutoff time.Time) (int64, error) {
	return s.queries.DeleteExpiredRefreshTokens(ctx, cutoff)
}

// RotateRefreshToken revokes the parent token and inserts the child in one
// transaction, returning the child row. The named atomic form of the SQL
// backend's InTx rotation so the auth service is queue/tx-agnostic.
func (s *Store) RotateRefreshToken(ctx context.Context, parentID uuid.UUID, child CreateRefreshTokenParams) (RefreshToken, error) {
	var row RefreshToken
	err := s.InTx(ctx, func(q *Queries) error {
		if err := q.RevokeRefreshToken(ctx, parentID); err != nil {
			return err
		}
		r, err := q.CreateRefreshToken(ctx, child)
		if err != nil {
			return err
		}
		row = r
		return nil
	})
	if err != nil {
		return RefreshToken{}, err
	}
	return row, nil
}
