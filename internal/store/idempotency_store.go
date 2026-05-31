// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import "context"

// DeleteExpiredIdempotencyKeys removes idempotency rows past their TTL,
// returning the count. Backs the periodic cleanup sweep.
func (s *Store) DeleteExpiredIdempotencyKeys(ctx context.Context) (int64, error) {
	return s.queries.DeleteExpiredIdempotencyKeys(ctx)
}

// GetIdempotencyKey returns the row for key, or ErrNotFound.
func (s *Store) GetIdempotencyKey(ctx context.Context, key string) (IdempotencyKey, error) {
	row, err := s.queries.GetIdempotencyKey(ctx, key)
	if err != nil {
		return IdempotencyKey{}, translateNoRows(err)
	}
	return row, nil
}

// BeginIdempotencyKey inserts a fresh in_flight row (ON CONFLICT DO NOTHING),
// returning ErrNotFound when another caller already claimed the key.
func (s *Store) BeginIdempotencyKey(ctx context.Context, arg BeginIdempotencyKeyParams) (IdempotencyKey, error) {
	row, err := s.queries.BeginIdempotencyKey(ctx, arg)
	if err != nil {
		return IdempotencyKey{}, translateNoRows(err)
	}
	return row, nil
}

// ReclaimIdempotencyKey overwrites an expired row in place, returning ErrNotFound
// when no expired row was claimed.
func (s *Store) ReclaimIdempotencyKey(ctx context.Context, arg ReclaimIdempotencyKeyParams) (IdempotencyKey, error) {
	row, err := s.queries.ReclaimIdempotencyKey(ctx, arg)
	if err != nil {
		return IdempotencyKey{}, translateNoRows(err)
	}
	return row, nil
}

// CompleteIdempotencyKey records the cached response and flips an in_flight row
// to completed.
func (s *Store) CompleteIdempotencyKey(ctx context.Context, arg CompleteIdempotencyKeyParams) error {
	return s.queries.CompleteIdempotencyKey(ctx, arg)
}

// DeleteIdempotencyKey removes an in_flight row (the non-2xx cleanup path).
func (s *Store) DeleteIdempotencyKey(ctx context.Context, key string) error {
	return s.queries.DeleteIdempotencyKey(ctx, key)
}
