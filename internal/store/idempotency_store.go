// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import "context"

// DeleteExpiredIdempotencyKeys removes idempotency rows past their TTL,
// returning the count. Backs the periodic cleanup sweep.
func (s *Store) DeleteExpiredIdempotencyKeys(ctx context.Context) (int64, error) {
	return s.queries.DeleteExpiredIdempotencyKeys(ctx)
}
