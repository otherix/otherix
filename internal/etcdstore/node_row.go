// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// casNodeUpdate re-reads a node, applies mutate, and commits the whole row under
// a ModRevision compare-and-set, retrying on a lost race so a concurrent writer
// (notably an operator gateway-role toggle) is never clobbered. A blind put
// that read the row before the toggle committed would re-persist the stale role
// bit and silently lose the toggle; the CAS-retry re-reads on conflict so every
// field, including a bit set between the read and the write, survives. mutate
// must be a pure field update on the passed row; it runs once per attempt on the
// freshest read and cannot abort - a status transition with a precondition is
// casNodeStatus's job, not this one. Returns store.ErrConcurrentUpdate after the
// retry bound is exhausted, and store.ErrNotFound for a missing or soft-deleted
// node.
func (s *Store) casNodeUpdate(ctx context.Context, id uuid.UUID, mutate func(*store.Node)) (store.Node, error) {
	const maxAttempts = 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		n, modRev, err := s.nodeWithRev(ctx, id)
		if err != nil {
			return store.Node{}, err
		}
		mutate(&n)
		val, err := etcd.Marshal(n)
		if err != nil {
			return store.Node{}, err
		}
		resp, err := s.c.Raw().Txn(ctx).
			If(clientv3.Compare(clientv3.ModRevision(nodeKey(id)), "=", modRev)).
			Then(clientv3.OpPut(nodeKey(id), string(val))).
			Commit()
		if err != nil {
			return store.Node{}, fmt.Errorf("cas node update txn: %v", err)
		}
		if resp.Succeeded {
			return n, nil
		}
	}
	return store.Node{}, store.ErrConcurrentUpdate
}
