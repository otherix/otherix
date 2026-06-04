// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"
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

// nextSeq atomically allocates the next value of a monotonic counter stored at
// key, via a value-compare CAS loop (absent key starts the sequence at 1).
func (s *Store) nextSeq(ctx context.Context, key string) (int64, error) {
	for range 64 {
		cur, found, err := s.c.Get(ctx, key)
		if err != nil {
			return 0, err
		}
		var n int64
		if found {
			n, err = strconv.ParseInt(string(cur), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("corrupt sequence %q: %v", key, err)
			}
		}
		next := n + 1
		var cmp clientv3.Cmp
		if found {
			cmp = clientv3.Compare(clientv3.Value(key), "=", string(cur))
		} else {
			cmp = clientv3.Compare(clientv3.CreateRevision(key), "=", 0)
		}
		resp, err := s.c.Raw().Txn(ctx).
			If(cmp).
			Then(clientv3.OpPut(key, strconv.FormatInt(next, 10))).
			Commit()
		if err != nil {
			return 0, fmt.Errorf("allocate sequence %q: %v", key, err)
		}
		if resp.Succeeded {
			return next, nil
		}
	}
	return 0, fmt.Errorf("etcdstore: sequence %q contention exceeded retry budget", key)
}
