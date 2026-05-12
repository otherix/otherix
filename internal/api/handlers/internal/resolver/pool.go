// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package resolver

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/otherix/otherix/internal/store"
)

// Pool looks up a single storage_pool instance by UUID literal or by
// name. Storage pool is the one resource that stays polymorphic
// across name-based narrowing, because the same pool name may exist
// on multiple nodes (one row per node) and per-instance operations
// (PATCH, DELETE, SCAN) require UUID addressing to disambiguate. Pool
// returns CodeAmbiguous when a name matches more than one instance -
// callers that want the per-node row must address it by UUID, callers
// that want the aggregated cluster view should call PoolByName
// instead.
func Pool(ctx context.Context, q Querier, identifier string) (store.StoragePool, error) {
	if id, err := uuid.Parse(identifier); err == nil {
		row, err := q.GetStoragePoolByID(ctx, id)
		if err != nil {
			return store.StoragePool{}, wrapLookupErr(KindPool, identifier, err)
		}
		return row, nil
	}
	rows, err := q.ListStoragePoolsByName(ctx, identifier)
	if err != nil {
		return store.StoragePool{}, wrapLookupErr(KindPool, identifier, err)
	}
	switch len(rows) {
	case 0:
		return store.StoragePool{}, wrapLookupErr(KindPool, identifier, pgx.ErrNoRows)
	case 1:
		return rows[0], nil
	default:
		return store.StoragePool{}, &Error{Kind: KindPool, Identifier: identifier, Code: CodeAmbiguous}
	}
}

// PoolConcept is the cluster-wide view of a pool name under the
// multi-instance model. Each Instances row is a per-node
// materialisation of the same conceptual pool; Type is taken from the
// first instance (the CHECK constraint guarantees uniform type across
// the cluster while only 'local_dir' is allowed, and validation
// widens with the type catalogue). IsClusterDefault reflects
// cluster_settings.default_pool_name.
type PoolConcept struct {
	Name             string
	Type             string
	IsClusterDefault bool
	Instances        []store.StoragePool
}

// PoolByName returns the aggregated cluster view of a pool name. Use
// this whenever the API surface speaks at the name level (e.g.
// GET /v1/storage-pools/{name}); use Pool when the caller already
// knows it wants a single per-node instance row.
func PoolByName(ctx context.Context, q Querier, name string) (PoolConcept, error) {
	rows, err := q.ListStoragePoolsByName(ctx, name)
	if err != nil {
		return PoolConcept{}, wrapLookupErr(KindPool, name, err)
	}
	if len(rows) == 0 {
		return PoolConcept{}, wrapLookupErr(KindPool, name, pgx.ErrNoRows)
	}
	settings, err := q.GetClusterSettings(ctx)
	if err != nil {
		return PoolConcept{}, wrapLookupErr(KindPool, name, err)
	}
	isDefault := settings.DefaultPoolName != nil &&
		strings.EqualFold(*settings.DefaultPoolName, rows[0].Name)
	return PoolConcept{
		Name:             rows[0].Name,
		Type:             rows[0].Type,
		IsClusterDefault: isDefault,
		Instances:        rows,
	}, nil
}
