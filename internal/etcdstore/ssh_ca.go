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

// The cluster SSH user-CA is keyed by UUID with an at-most-one-active guard key,
// mirroring the cluster CA layout in ca_certs.go. The guard value is the active
// row's id, so it doubles as the lookup index for ActiveSSHUserCA. CreateSSHUserCA
// asserts the guard absent (compare-and-set) so concurrent HA replicas converge
// on a single CA: the loser of the race fetches and returns the winner's row.

func sshUserCAKey(id uuid.UUID) string { return etcd.Key("ssh_ca", id.String()) }

func sshUserCAActiveGuard() string { return etcd.Key("uniq", "ssh_ca", "active") }

// ActiveSSHUserCA returns the active cluster SSH user-CA row, or
// store.ErrNotFound when no SSH user-CA has been provisioned.
func (s *Store) ActiveSSHUserCA(ctx context.Context) (store.SSHUserCA, error) {
	idBytes, found, err := s.c.Get(ctx, sshUserCAActiveGuard())
	if err != nil {
		return store.SSHUserCA{}, err
	}
	if !found {
		return store.SSHUserCA{}, store.ErrNotFound
	}
	id, err := uuid.Parse(string(idBytes))
	if err != nil {
		return store.SSHUserCA{}, fmt.Errorf("corrupt active SSH user CA guard: %v", err)
	}
	var ca store.SSHUserCA
	ok, err := s.c.GetJSON(ctx, sshUserCAKey(id), &ca)
	if err != nil {
		return store.SSHUserCA{}, err
	}
	if !ok {
		return store.SSHUserCA{}, store.ErrNotFound
	}
	return ca, nil
}

// CreateSSHUserCA inserts the cluster SSH user-CA, stamping a fresh id and
// created_at and writing the primary row plus the active guard atomically. It is
// race-safe across concurrent HA replicas: the compare-and-set guard admits one
// winner, and a losing caller refetches and returns the winner's row (nil error)
// so every replica converges on a single signer.
func (s *Store) CreateSSHUserCA(ctx context.Context, arg store.CreateSSHUserCAParams) (store.SSHUserCA, error) {
	ca := store.SSHUserCA{
		ID:                  uuid.New(),
		PrivateKeyPEM:       arg.PrivateKeyPEM,
		PublicKeyAuthorized: arg.PublicKeyAuthorized,
		CreatedAt:           time.Now().UTC(),
	}
	val, err := etcd.Marshal(ca)
	if err != nil {
		return store.SSHUserCA{}, err
	}
	guard := sshUserCAActiveGuard()
	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(guard), "=", 0)).
		Then(
			clientv3.OpPut(guard, ca.ID.String()),
			clientv3.OpPut(sshUserCAKey(ca.ID), string(val)),
		).
		Commit()
	if err != nil {
		return store.SSHUserCA{}, fmt.Errorf("create SSH user CA txn: %v", err)
	}
	if !resp.Succeeded {
		// A sibling replica created the active row first. Return the winner's
		// row so every replica loads the same SSH user-CA.
		winner, err := s.ActiveSSHUserCA(ctx)
		if err != nil {
			return store.SSHUserCA{}, fmt.Errorf("lost SSH user CA create race; refetch failed: %v", err)
		}
		return winner, nil
	}
	return ca, nil
}
