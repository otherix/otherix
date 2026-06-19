// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// Artifact pools are a bounded, cluster-wide collection addressed by UUID with a
// case-insensitive name uniqueness guard. The name namespace is shared with
// storage pools (a pool name denotes exactly one kind), enforced cross-namespace
// on both create paths. The list is a primary-prefix scan (small, cold path),
// mirroring ListNetworks.

func artifactPoolKey(id uuid.UUID) string { return etcd.Key("artifact_pools", id.String()) }

func artifactPoolPrefix() string { return etcd.Key("artifact_pools") + "/" }

func artifactPoolNameGuard(name string) string {
	return etcd.Key("uniq", "artifact_pools", "name", strings.ToLower(name))
}

// CreateArtifactPool inserts an artifact pool, stamping timestamps, writing the
// name guard + primary atomically. A same-name live artifact pool returns
// store.ErrArtifactPoolNameExists. A name already used by a storage pool (the
// other namespace) returns store.ErrPoolNameConflict via a pre-check (the rare
// concurrent cross-namespace create of the same name is an accepted, documented
// race - the same class as other create races in this codebase).
func (s *Store) CreateArtifactPool(ctx context.Context, p store.CreateArtifactPoolParams) (store.ArtifactPool, error) {
	if disk, err := s.StoragePoolsByName(ctx, p.Name); err != nil {
		return store.ArtifactPool{}, err
	} else if len(disk) > 0 {
		return store.ArtifactPool{}, store.ErrPoolNameConflict
	}

	now := time.Now().UTC()
	ap := store.ArtifactPool{
		ID:                p.ID,
		Name:              p.Name,
		ReplicationFactor: p.ReplicationFactor,
		Membership:        p.Membership,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	val, err := etcd.Marshal(ap)
	if err != nil {
		return store.ArtifactPool{}, err
	}
	guard := artifactPoolNameGuard(ap.Name)
	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(guard), "=", 0)).
		Then(
			clientv3.OpPut(guard, ap.ID.String()),
			clientv3.OpPut(artifactPoolKey(ap.ID), string(val)),
		).
		Commit()
	if err != nil {
		return store.ArtifactPool{}, fmt.Errorf("create artifact pool txn: %v", err)
	}
	if !resp.Succeeded {
		return store.ArtifactPool{}, store.ErrArtifactPoolNameExists
	}
	return ap, nil
}

// ArtifactPoolByID returns the non-deleted artifact pool, or store.ErrNotFound.
func (s *Store) ArtifactPoolByID(ctx context.Context, id uuid.UUID) (store.ArtifactPool, error) {
	var ap store.ArtifactPool
	found, err := s.c.GetJSON(ctx, artifactPoolKey(id), &ap)
	if err != nil {
		return store.ArtifactPool{}, err
	}
	if !found || ap.DeletedAt != nil {
		return store.ArtifactPool{}, store.ErrNotFound
	}
	return ap, nil
}

// ArtifactPoolByName resolves a non-deleted artifact pool through its
// case-insensitive name guard, returning store.ErrNotFound when none owns it.
func (s *Store) ArtifactPoolByName(ctx context.Context, name string) (store.ArtifactPool, error) {
	val, found, err := s.c.Get(ctx, artifactPoolNameGuard(name))
	if err != nil {
		return store.ArtifactPool{}, err
	}
	if !found {
		return store.ArtifactPool{}, store.ErrNotFound
	}
	id, err := uuid.Parse(string(val))
	if err != nil {
		return store.ArtifactPool{}, fmt.Errorf("corrupt artifact pool name guard %q: %v", name, err)
	}
	return s.ArtifactPoolByID(ctx, id)
}

// UpdateArtifactPool applies the non-nil fields of params to the artifact pool
// with the given id, bumps updated_at, and returns the updated row. The name is
// immutable (no params field) so the name guard is left untouched and the row
// is rewritten in place by id. Returns store.ErrNotFound when the pool is absent
// or deleted.
func (s *Store) UpdateArtifactPool(ctx context.Context, id uuid.UUID, params store.UpdateArtifactPoolParams) (store.ArtifactPool, error) {
	ap, err := s.ArtifactPoolByID(ctx, id)
	if err != nil {
		return store.ArtifactPool{}, err
	}
	if params.ReplicationFactor != nil {
		ap.ReplicationFactor = *params.ReplicationFactor
	}
	if params.Membership != nil {
		ap.Membership = *params.Membership
	}
	ap.UpdatedAt = time.Now().UTC()
	val, err := etcd.Marshal(ap)
	if err != nil {
		return store.ArtifactPool{}, err
	}
	if err := s.c.Put(ctx, artifactPoolKey(ap.ID), val); err != nil {
		return store.ArtifactPool{}, fmt.Errorf("update artifact pool put: %v", err)
	}
	return ap, nil
}

// ListArtifactPools returns non-deleted artifact pools ordered by (created_at,
// id) ascending, after the cursor, capped at LimitCount. Bounded collection ->
// primary-prefix scan (mirrors ListNetworks).
func (s *Store) ListArtifactPools(ctx context.Context, p store.ListArtifactPoolsParams) ([]store.ArtifactPool, error) {
	items, err := s.c.Range(ctx, artifactPoolPrefix())
	if err != nil {
		return nil, err
	}
	pools := make([]store.ArtifactPool, 0, len(items))
	for _, kv := range items {
		var ap store.ArtifactPool
		if err := json.Unmarshal(kv.Value, &ap); err != nil {
			return nil, fmt.Errorf("unmarshal artifact pool %q: %v", kv.Key, err)
		}
		if ap.DeletedAt != nil {
			continue
		}
		if !afterCursor(ap.CreatedAt, ap.ID, p.CursorCreatedAt, p.CursorID) {
			continue
		}
		pools = append(pools, ap)
	}
	sort.Slice(pools, func(i, j int) bool {
		if !pools[i].CreatedAt.Equal(pools[j].CreatedAt) {
			return pools[i].CreatedAt.Before(pools[j].CreatedAt)
		}
		return pools[i].ID.String() < pools[j].ID.String()
	})
	if n := int(p.LimitCount); n > 0 && len(pools) > n {
		pools = pools[:n]
	}
	return pools, nil
}

// DeleteArtifactPool soft-deletes the artifact pool (drops the name guard so the
// name is reusable) after verifying no non-deleted snapshot is tagged with it.
// Returns store.ErrNotFound when missing, or *store.ResourceInUseError
// (key "snapshots") when referenced. Fail-closed: it counts FIRST and refuses on
// count>0, never deleting a still-referenced pool.
func (s *Store) DeleteArtifactPool(ctx context.Context, id uuid.UUID) error {
	existing, err := s.ArtifactPoolByID(ctx, id)
	if err != nil {
		return err
	}
	count, err := s.CountSnapshotsInArtifactPool(ctx, existing.Name)
	if err != nil {
		return err
	}
	if count > 0 {
		return &store.ResourceInUseError{Resources: map[string]int64{"snapshots": count}}
	}
	now := time.Now().UTC()
	existing.DeletedAt = &now
	existing.UpdatedAt = now
	val, err := etcd.Marshal(existing)
	if err != nil {
		return err
	}
	guard := artifactPoolNameGuard(existing.Name)
	rowOp := clientv3.OpPut(artifactPoolKey(id), string(val))
	// Drop the name guard ONLY if it still points at this pool. A concurrent
	// delete may have already freed the name and a new pool re-taken it; deleting
	// that guard would orphan the new pool's name. The row soft-delete runs in
	// both branches (id-keyed, no cross-pool race).
	if _, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.Value(guard), "=", id.String())).
		Then(rowOp, clientv3.OpDelete(guard)).
		Else(rowOp).
		Commit(); err != nil {
		return fmt.Errorf("delete artifact pool txn: %v", err)
	}
	return nil
}
