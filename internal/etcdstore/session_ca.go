// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// The ingress-session CA is keyed by UUID with an at-most-one-active guard key,
// mirroring the cluster SSH user-CA layout in ssh_ca.go. The guard value is the
// active row's id, so it doubles as the lookup index for ActiveSessionCA.
// CreateSessionCA asserts the guard absent (compare-and-set) so concurrent HA
// replicas converge on a single CA: the loser of the race fetches and returns
// the winner's row.

func sessionCAKey(id uuid.UUID) string { return etcd.Key("session_ca", id.String()) }

func sessionCAActiveGuard() string { return etcd.Key("uniq", "session_ca", "active") }

// ActiveSessionCA returns the active ingress-session CA row, or
// store.ErrNotFound when no session CA has been provisioned.
func (s *Store) ActiveSessionCA(ctx context.Context) (store.SessionCA, error) {
	idBytes, found, err := s.c.Get(ctx, sessionCAActiveGuard())
	if err != nil {
		return store.SessionCA{}, err
	}
	if !found {
		return store.SessionCA{}, store.ErrNotFound
	}
	id, err := uuid.Parse(string(idBytes))
	if err != nil {
		return store.SessionCA{}, fmt.Errorf("corrupt active session CA guard: %v", err)
	}
	var ca store.SessionCA
	ok, err := s.c.GetJSON(ctx, sessionCAKey(id), &ca)
	if err != nil {
		return store.SessionCA{}, err
	}
	if !ok {
		return store.SessionCA{}, store.ErrNotFound
	}
	return ca, nil
}

// CreateSessionCA inserts the ingress-session CA, stamping a fresh id and
// created_at and writing the primary row plus the active guard atomically. It is
// race-safe across concurrent HA replicas: the compare-and-set guard admits one
// winner, and a losing caller refetches and returns the winner's row (nil error)
// so every replica converges on a single signer.
func (s *Store) CreateSessionCA(ctx context.Context, arg store.CreateSessionCAParams) (store.SessionCA, error) {
	ca := store.SessionCA{
		ID:            uuid.New(),
		PrivateKeyPEM: arg.PrivateKeyPEM,
		PublicKeyPEM:  arg.PublicKeyPEM,
		CreatedAt:     time.Now().UTC(),
	}
	val, err := etcd.Marshal(ca)
	if err != nil {
		return store.SessionCA{}, err
	}
	guard := sessionCAActiveGuard()
	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(guard), "=", 0)).
		Then(
			clientv3.OpPut(guard, ca.ID.String()),
			clientv3.OpPut(sessionCAKey(ca.ID), string(val)),
		).
		Commit()
	if err != nil {
		return store.SessionCA{}, fmt.Errorf("create session CA txn: %v", err)
	}
	if !resp.Succeeded {
		// A sibling replica created the active row first. Return the winner's
		// row so every replica loads the same session CA.
		winner, err := s.ActiveSessionCA(ctx)
		if err != nil {
			return store.SessionCA{}, fmt.Errorf("lost session CA create race; refetch failed: %v", err)
		}
		return winner, nil
	}
	return ca, nil
}
