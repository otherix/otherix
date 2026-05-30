// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// Cluster CA certs are keyed by UUID, with an at-most-one-active guard key that
// replaces the SQL uq_ca_certs_active partial unique index. The guard value is
// the active row's id, so it doubles as the lookup index for ActiveCACert.
// CreateCACert asserts the guard absent (compare-and-set) and returns
// store.ErrCACertActiveExists on a lost race, matching the SQL 23505 path the
// CA bootstrap traps.

func caCertKey(id uuid.UUID) string { return etcd.Key("ca_certs", id.String()) }

func caCertActiveGuard() string { return etcd.Key("uniq", "ca_certs", "active") }

// ActiveCACert returns the active cluster CA row, or store.ErrNotFound when no
// active CA has been provisioned.
func (s *Store) ActiveCACert(ctx context.Context) (store.CaCert, error) {
	idBytes, found, err := s.c.Get(ctx, caCertActiveGuard())
	if err != nil {
		return store.CaCert{}, err
	}
	if !found {
		return store.CaCert{}, store.ErrNotFound
	}
	id, err := uuid.Parse(string(idBytes))
	if err != nil {
		return store.CaCert{}, fmt.Errorf("corrupt active CA guard: %v", err)
	}
	var ca store.CaCert
	ok, err := s.c.GetJSON(ctx, caCertKey(id), &ca)
	if err != nil {
		return store.CaCert{}, err
	}
	if !ok || !ca.Active {
		return store.CaCert{}, store.ErrNotFound
	}
	return ca, nil
}

// CreateCACert inserts a new active cluster CA row, stamping created_at and
// active=true, writing the primary + active guard atomically. A second active
// insert returns store.ErrCACertActiveExists (the lost-race signal the CA
// bootstrap traps to fetch the winner).
func (s *Store) CreateCACert(ctx context.Context, arg store.CreateCACertParams) (store.CaCert, error) {
	ca := store.CaCert{
		ID:                arg.ID,
		CertPem:           arg.CertPem,
		KeyPem:            arg.KeyPem,
		FingerprintSha256: arg.FingerprintSha256,
		NotBefore:         arg.NotBefore,
		NotAfter:          arg.NotAfter,
		Active:            true,
		CreatedAt:         time.Now().UTC(),
	}
	val, err := etcd.Marshal(ca)
	if err != nil {
		return store.CaCert{}, err
	}
	guard := caCertActiveGuard()
	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(guard), "=", 0)).
		Then(
			clientv3.OpPut(guard, ca.ID.String()),
			clientv3.OpPut(caCertKey(ca.ID), string(val)),
		).
		Commit()
	if err != nil {
		return store.CaCert{}, fmt.Errorf("create CA cert txn: %v", err)
	}
	if !resp.Succeeded {
		return store.CaCert{}, store.ErrCACertActiveExists
	}
	return ca, nil
}

// DeactivateCACerts marks the active CA row inactive and drops the active
// guard, allowing a fresh active row to be created (the future rotation step).
// Idempotent: clearing when no active row exists is a no-op.
func (s *Store) DeactivateCACerts(ctx context.Context) error {
	active, err := s.ActiveCACert(ctx)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	active.Active = false
	val, err := etcd.Marshal(active)
	if err != nil {
		return err
	}
	if _, err := s.c.Raw().Txn(ctx).
		Then(
			clientv3.OpPut(caCertKey(active.ID), string(val)),
			clientv3.OpDelete(caCertActiveGuard()),
		).
		Commit(); err != nil {
		return fmt.Errorf("deactivate CA certs txn: %v", err)
	}
	return nil
}
