// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrNetworkNameExists is returned when a network with the same name
// already exists (uq_networks_name).
var ErrNetworkNameExists = errors.New("store: network name already in use")

// translateNetworkUnique maps a unique-constraint violation on the
// networks table to ErrNetworkNameExists; any other error passes
// through. The table carries a single unique constraint (name), so any
// 23505 maps to the name conflict.
func translateNetworkUnique(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrNetworkNameExists
	}
	return err
}

// NetworkByID returns the network with the given id, or ErrNotFound.
func (s *Store) NetworkByID(ctx context.Context, id uuid.UUID) (Network, error) {
	n, err := s.queries.GetNetworkByID(ctx, id)
	if err != nil {
		return Network{}, translateNoRows(err)
	}
	return n, nil
}

// CreateNetwork inserts a network, translating the name-uniqueness
// violation to ErrNetworkNameExists.
func (s *Store) CreateNetwork(ctx context.Context, arg CreateNetworkParams) (Network, error) {
	n, err := s.queries.CreateNetwork(ctx, arg)
	if err != nil {
		return Network{}, translateNetworkUnique(err)
	}
	return n, nil
}

// UpdateNetwork updates a network, translating the name-uniqueness
// violation to ErrNetworkNameExists.
func (s *Store) UpdateNetwork(ctx context.Context, arg UpdateNetworkParams) (Network, error) {
	n, err := s.queries.UpdateNetwork(ctx, arg)
	if err != nil {
		return Network{}, translateNetworkUnique(err)
	}
	return n, nil
}

// ListNetworks returns networks matching the cursor-paginated filters
// in arg.
func (s *Store) ListNetworks(ctx context.Context, arg ListNetworksParams) ([]Network, error) {
	return s.queries.ListNetworks(ctx, arg)
}

// DeleteNetwork soft-deletes the network in a single transaction after
// verifying it exists and is unreferenced. Returns ErrNotFound when the
// row is missing, or *ResourceInUseError (key "vm_nics") when active
// vm_nics still reference it.
func (s *Store) DeleteNetwork(ctx context.Context, id uuid.UUID) error {
	return s.InTx(ctx, func(q *Queries) error {
		if _, err := q.GetNetworkByID(ctx, id); err != nil {
			return translateNoRows(err)
		}
		nicCount, err := q.CountVMNicsOnNetwork(ctx, id)
		if err != nil {
			return err
		}
		if nicCount > 0 {
			return &ResourceInUseError{Resources: map[string]int64{"vm_nics": nicCount}}
		}
		return q.SoftDeleteNetwork(ctx, id)
	})
}
