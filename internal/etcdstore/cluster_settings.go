// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"time"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// Cluster settings are a singleton (the SQL schema seeds one row at id=1 with a
// NULL default_pool_name). There is no etcd equivalent of the seed migration,
// so ClusterSettings materialises the default on read when the key is absent -
// matching the SQL contract that GetClusterSettings never returns not-found in
// a healthy schema. Writes upsert the singleton key.

func clusterSettingsKey() string { return etcd.Key("cluster_settings", "singleton") }

// ClusterSettings returns the singleton cluster-settings row, materialising the
// default (id=1, no default pool) when the key has never been written.
func (s *Store) ClusterSettings(ctx context.Context) (store.ClusterSetting, error) {
	var cs store.ClusterSetting
	found, err := s.c.GetJSON(ctx, clusterSettingsKey(), &cs)
	if err != nil {
		return store.ClusterSetting{}, err
	}
	if !found {
		now := time.Now().UTC()
		return store.ClusterSetting{ID: 1, CreatedAt: now, UpdatedAt: now}, nil
	}
	return cs, nil
}

// SetDefaultPoolName writes the cluster-wide default pool name on the singleton,
// upserting the row and bumping updated_at. The caller validates that the name
// resolves to at least one existing pool instance before calling.
func (s *Store) SetDefaultPoolName(ctx context.Context, name *string) error {
	return s.writeClusterSettings(ctx, name)
}

// ClearDefaultPoolName nulls the cluster-wide default pool name. Idempotent.
func (s *Store) ClearDefaultPoolName(ctx context.Context) error {
	return s.writeClusterSettings(ctx, nil)
}

// writeClusterSettings upserts the singleton with the given default pool name,
// preserving created_at when the row already exists.
func (s *Store) writeClusterSettings(ctx context.Context, name *string) error {
	cur, err := s.ClusterSettings(ctx)
	if err != nil {
		return err
	}
	cur.ID = 1
	cur.DefaultPoolName = name
	if cur.CreatedAt.IsZero() {
		cur.CreatedAt = time.Now().UTC()
	}
	cur.UpdatedAt = time.Now().UTC()
	return s.c.PutJSON(ctx, clusterSettingsKey(), cur)
}
