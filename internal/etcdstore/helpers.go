// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// resolveGuard reads a uniqueness/index guard key whose value is a UUID and
// parses it. found is false (nil error) when the guard is absent. Guards double
// as name->id / unique->id indexes, so this is the shared lookup primitive
// behind the *ByName / *ByEmail style methods.
func (s *Store) resolveGuard(ctx context.Context, key string) (id uuid.UUID, found bool, err error) {
	raw, ok, err := s.c.Get(ctx, key)
	if err != nil {
		return uuid.Nil, false, err
	}
	if !ok {
		return uuid.Nil, false, nil
	}
	parsed, err := uuid.Parse(string(raw))
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("corrupt guard %q: %v", key, err)
	}
	return parsed, true, nil
}
