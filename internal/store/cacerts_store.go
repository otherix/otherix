// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// ActiveCACert returns the active cluster CA row, or ErrNotFound when no
// active CA has been provisioned. In production the boot-time
// BootstrapClusterCA hook guarantees exactly one active row exists, so
// ErrNotFound here signals an operational invariant violation that
// callers surface as 500.
func (s *Store) ActiveCACert(ctx context.Context) (CaCert, error) {
	row, err := s.queries.GetActiveCACert(ctx)
	if err != nil {
		return CaCert{}, translateNoRows(err)
	}
	return row, nil
}

// CreateCACert inserts a CA row, translating the uq_ca_certs_active partial
// unique violation to ErrCACertActiveExists so callers can detect a lost
// bootstrap race backend-neutrally.
func (s *Store) CreateCACert(ctx context.Context, arg CreateCACertParams) (CaCert, error) {
	row, err := s.queries.CreateCACert(ctx, arg)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return CaCert{}, ErrCACertActiveExists
		}
		return CaCert{}, err
	}
	return row, nil
}
