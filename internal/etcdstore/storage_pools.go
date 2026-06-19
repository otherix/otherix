// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// Storage pools are multi-instance: the same name may exist once per
// node, so uniqueness is scoped (node_id, lower(name)) via a per-node guard,
// and a cluster-wide name index backs the name-aggregated lookups. Effective
// capacity subtracts pending vm_disk commits (disks on the pool created after
// the last scan). The observed per-pool image inventory is heartbeat-fed
// observed state at a single per-pool key (last-writer-wins, no guard).

const bytesPerGiB = 1073741824

func storagePoolKey(id uuid.UUID) string { return etcd.Key("storage_pools", id.String()) }

func storagePoolPrefix() string { return etcd.Key("storage_pools") + "/" }

func storagePoolNodeNameGuard(nodeID uuid.UUID, name string) string {
	return etcd.Key("uniq", "storage_pools", "node_name", nodeID.String(), strings.ToLower(name))
}

func storagePoolNameIndexKey(name string, id uuid.UUID) string {
	return etcd.Key("index", "storage_pools", "name", strings.ToLower(name), id.String())
}

func storagePoolNameIndexPrefix(name string) string {
	return etcd.Key("index", "storage_pools", "name", strings.ToLower(name)) + "/"
}

func poolImageInventoryKey(poolID uuid.UUID) string {
	return etcd.Key("pool_images", poolID.String())
}

// vmDiskKey is the canonical primary key for vm_disks (reused by the vms slice).
func vmDiskKey(id uuid.UUID) string { return etcd.Key("vm_disks", id.String()) }

// vmDisksPoolIndexPrefix lists the vm_disks on a pool (maintained by the vms
// slice) - consumed by the pool delete block + effective-capacity pending term.
func vmDisksPoolIndexPrefix(poolID uuid.UUID) string {
	return etcd.Key("index", "vm_disks", "pool", poolID.String()) + "/"
}

// StoragePoolByID returns the non-deleted pool instance with the given id, or
// store.ErrNotFound.
func (s *Store) StoragePoolByID(ctx context.Context, id uuid.UUID) (store.StoragePool, error) {
	var p store.StoragePool
	found, err := s.c.GetJSON(ctx, storagePoolKey(id), &p)
	if err != nil {
		return store.StoragePool{}, err
	}
	if !found || p.DeletedAt != nil {
		return store.StoragePool{}, store.ErrNotFound
	}
	return p, nil
}

// StoragePoolsByName returns every non-deleted per-node instance sharing the
// given name (case-insensitive), ordered by node_id. An empty result is not an
// error.
func (s *Store) StoragePoolsByName(ctx context.Context, name string) ([]store.StoragePool, error) {
	items, err := s.c.Range(ctx, storagePoolNameIndexPrefix(name))
	if err != nil {
		return nil, err
	}
	pools := make([]store.StoragePool, 0, len(items))
	for _, kv := range items {
		id, perr := uuid.Parse(string(kv.Value))
		if perr != nil {
			return nil, fmt.Errorf("corrupt pool name index %q: %v", kv.Key, perr)
		}
		p, err := s.StoragePoolByID(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return nil, err
		}
		pools = append(pools, p)
	}
	sort.Slice(pools, func(i, j int) bool { return pools[i].NodeID.String() < pools[j].NodeID.String() })
	return pools, nil
}

// CreateStoragePool inserts a pool, stamping created_at/updated_at and writing
// the primary + per-node guard + cluster name index atomically. A per-node name
// collision returns store.ErrStoragePoolNameExists.
func (s *Store) CreateStoragePool(ctx context.Context, arg store.CreateStoragePoolParams) (store.StoragePool, error) {
	// Cross-namespace pre-check: the name must not belong to an artifact pool
	// (a pool name denotes exactly one kind). The rare concurrent cross-namespace
	// create of the same name is an accepted, documented race.
	if _, err := s.ArtifactPoolByName(ctx, arg.Name); err == nil {
		return store.StoragePool{}, store.ErrPoolNameConflict
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.StoragePool{}, err
	}

	now := time.Now().UTC()
	p := store.StoragePool{
		ID:                   arg.ID,
		NodeID:               arg.NodeID,
		Name:                 arg.Name,
		Type:                 arg.Type,
		Path:                 arg.Path,
		Config:               arg.Config,
		ReconciliationStatus: "pending",
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	val, err := etcd.Marshal(p)
	if err != nil {
		return store.StoragePool{}, err
	}
	guard := storagePoolNodeNameGuard(p.NodeID, p.Name)
	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(guard), "=", 0)).
		Then(
			clientv3.OpPut(guard, p.ID.String()),
			clientv3.OpPut(storagePoolKey(p.ID), string(val)),
			clientv3.OpPut(storagePoolNameIndexKey(p.Name, p.ID), p.ID.String()),
		).
		Commit()
	if err != nil {
		return store.StoragePool{}, fmt.Errorf("create storage pool txn: %v", err)
	}
	if !resp.Succeeded {
		return store.StoragePool{}, store.ErrStoragePoolNameExists
	}
	return p, nil
}

// UpdateStoragePool rewrites name + config (node_id/type/path are immutable),
// bumps updated_at, and moves the per-node guard + name index on rename.
// Returns store.ErrNotFound or store.ErrStoragePoolNameExists.
func (s *Store) UpdateStoragePool(ctx context.Context, arg store.UpdateStoragePoolParams) (store.StoragePool, error) {
	existing, err := s.StoragePoolByID(ctx, arg.ID)
	if err != nil {
		return store.StoragePool{}, err
	}
	updated := existing
	updated.Name = arg.Name
	updated.Config = arg.Config
	updated.UpdatedAt = time.Now().UTC()

	val, err := etcd.Marshal(updated)
	if err != nil {
		return store.StoragePool{}, err
	}

	oldGuard := storagePoolNodeNameGuard(existing.NodeID, existing.Name)
	newGuard := storagePoolNodeNameGuard(existing.NodeID, arg.Name)
	if oldGuard == newGuard {
		if err := s.c.Put(ctx, storagePoolKey(arg.ID), val); err != nil {
			return store.StoragePool{}, err
		}
		return updated, nil
	}
	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(newGuard), "=", 0)).
		Then(
			clientv3.OpPut(newGuard, arg.ID.String()),
			clientv3.OpDelete(oldGuard),
			clientv3.OpDelete(storagePoolNameIndexKey(existing.Name, arg.ID)),
			clientv3.OpPut(storagePoolNameIndexKey(arg.Name, arg.ID), arg.ID.String()),
			clientv3.OpPut(storagePoolKey(arg.ID), string(val)),
		).
		Commit()
	if err != nil {
		return store.StoragePool{}, fmt.Errorf("update storage pool txn: %v", err)
	}
	if !resp.Succeeded {
		return store.StoragePool{}, store.ErrStoragePoolNameExists
	}
	return updated, nil
}

// PoolEffectiveByID returns the pool joined with its effective-capacity view,
// or store.ErrNotFound.
func (s *Store) PoolEffectiveByID(ctx context.Context, id uuid.UUID) (store.PoolEffectiveCapacity, error) {
	p, err := s.StoragePoolByID(ctx, id)
	if err != nil {
		return store.PoolEffectiveCapacity{}, err
	}
	return s.poolEffective(ctx, p)
}

// ListPoolsEffective returns pools matching the optional node_id/type filters,
// each joined with effective capacity, ordered by (created_at, id) ascending,
// after the cursor, capped at LimitCount.
func (s *Store) ListPoolsEffective(ctx context.Context, arg store.ListPoolsEffectiveParams) ([]store.PoolEffectiveCapacity, error) {
	items, err := s.c.Range(ctx, storagePoolPrefix())
	if err != nil {
		return nil, err
	}
	out := make([]store.PoolEffectiveCapacity, 0, len(items))
	for _, kv := range items {
		var p store.StoragePool
		if err := json.Unmarshal(kv.Value, &p); err != nil {
			return nil, fmt.Errorf("unmarshal storage pool %q: %v", kv.Key, err)
		}
		if p.DeletedAt != nil {
			continue
		}
		if arg.NodeID != nil && p.NodeID != *arg.NodeID {
			continue
		}
		if arg.Type != nil && p.Type != *arg.Type {
			continue
		}
		if !afterCursor(p.CreatedAt, p.ID, arg.CursorCreatedAt, arg.CursorID) {
			continue
		}
		eff, err := s.poolEffective(ctx, p)
		if err != nil {
			return nil, err
		}
		out = append(out, eff)
	}
	sortByCreatedAtID(out, func(e store.PoolEffectiveCapacity) (time.Time, uuid.UUID) { return e.CreatedAt, e.ID })
	if n := int(arg.LimitCount); n > 0 && len(out) > n {
		out = out[:n]
	}
	return out, nil
}

// ListPoolsEffectiveByName returns every non-deleted instance sharing the name,
// each joined with effective capacity, ordered by node_id.
func (s *Store) ListPoolsEffectiveByName(ctx context.Context, name string) ([]store.PoolEffectiveCapacity, error) {
	pools, err := s.StoragePoolsByName(ctx, name)
	if err != nil {
		return nil, err
	}
	out := make([]store.PoolEffectiveCapacity, 0, len(pools))
	for _, p := range pools {
		eff, err := s.poolEffective(ctx, p)
		if err != nil {
			return nil, err
		}
		out = append(out, eff)
	}
	return out, nil
}

// DeleteStoragePool soft-deletes a pool after verifying nothing references it.
// Returns store.ErrNotFound when missing, or *store.ResourceInUseError (key
// "vm_disks") when active disks block the delete. Image cache files are
// agent-owned and never block a pool delete.
func (s *Store) DeleteStoragePool(ctx context.Context, id uuid.UUID) error {
	p, err := s.StoragePoolByID(ctx, id)
	if err != nil {
		return err
	}
	diskCount, err := s.countPrefix(ctx, vmDisksPoolIndexPrefix(id))
	if err != nil {
		return err
	}
	if diskCount > 0 {
		return &store.ResourceInUseError{Resources: map[string]int64{"vm_disks": diskCount}}
	}
	now := time.Now().UTC()
	p.DeletedAt = &now
	p.UpdatedAt = now
	val, err := etcd.Marshal(p)
	if err != nil {
		return err
	}
	if _, err := s.c.Raw().Txn(ctx).
		Then(
			clientv3.OpPut(storagePoolKey(id), string(val)),
			clientv3.OpDelete(storagePoolNodeNameGuard(p.NodeID, p.Name)),
			clientv3.OpDelete(storagePoolNameIndexKey(p.Name, id)),
			// Drop the agent-reported image inventory (observed state); a
			// deleted pool reports none, and nothing reads it post-delete.
			clientv3.OpDelete(poolImageInventoryKey(id)),
		).
		Commit(); err != nil {
		return fmt.Errorf("delete storage pool txn: %v", err)
	}
	return nil
}

// UpsertPoolImageInventory replaces the observed image inventory for a pool
// with images. Observed state written from the heartbeat path: a blind put,
// last-writer-wins per heartbeat. An empty slice clears the inventory so a pool
// that dropped all images reports empty, not stale.
func (s *Store) UpsertPoolImageInventory(ctx context.Context, poolID uuid.UUID, images []store.PoolImage) error {
	if len(images) == 0 {
		_, err := s.c.Delete(ctx, poolImageInventoryKey(poolID))
		return err
	}
	return s.c.PutJSON(ctx, poolImageInventoryKey(poolID), images)
}

// PoolImageInventory returns the observed image inventory for a pool, or an
// empty slice when none was reported yet.
func (s *Store) PoolImageInventory(ctx context.Context, poolID uuid.UUID) ([]store.PoolImage, error) {
	var out []store.PoolImage
	found, err := s.c.GetJSON(ctx, poolImageInventoryKey(poolID), &out)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return out, nil
}

// poolEffective projects a pool onto the effective-capacity view, subtracting
// pending vm_disk commits from available_bytes.
func (s *Store) poolEffective(ctx context.Context, p store.StoragePool) (store.PoolEffectiveCapacity, error) {
	e := store.PoolEffectiveCapacity{
		ID:                   p.ID,
		NodeID:               p.NodeID,
		Name:                 p.Name,
		Type:                 p.Type,
		Path:                 p.Path,
		CapacityBytes:        p.CapacityBytes,
		AvailableBytes:       p.AvailableBytes,
		ReportedAt:           p.ReportedAt,
		Config:               p.Config,
		DiskPressureSince:    p.DiskPressureSince,
		DiskPressureCount:    p.DiskPressureCount,
		ReconciliationStatus: p.ReconciliationStatus,
		LastReconciledAt:     p.LastReconciledAt,
		ReconciliationError:  p.ReconciliationError,
		CreatedAt:            p.CreatedAt,
		UpdatedAt:            p.UpdatedAt,
		DeletedAt:            p.DeletedAt,
	}
	if p.AvailableBytes != nil {
		pending, err := s.pendingCommittedBytes(ctx, p)
		if err != nil {
			return store.PoolEffectiveCapacity{}, err
		}
		v := max(*p.AvailableBytes-pending, 0)
		e.AvailableBytesEffective = &v
	}
	return e, nil
}

// pendingCommittedBytes sums the bytes of vm_disks on the pool the agent has not
// yet observed (created after the pool's last scan), matching the lateral
// subquery of pool_effective_capacity.
func (s *Store) pendingCommittedBytes(ctx context.Context, p store.StoragePool) (int64, error) {
	items, err := s.c.Range(ctx, vmDisksPoolIndexPrefix(p.ID))
	if err != nil {
		return 0, err
	}
	var total int64
	for _, kv := range items {
		id, perr := uuid.Parse(string(kv.Value))
		if perr != nil {
			return 0, fmt.Errorf("corrupt vm_disk pool index %q: %v", kv.Key, perr)
		}
		var d store.VMDisk
		found, gerr := s.c.GetJSON(ctx, vmDiskKey(id), &d)
		if gerr != nil {
			return 0, gerr
		}
		if !found || d.DeletedAt != nil {
			continue
		}
		if p.ReportedAt != nil && !d.CreatedAt.After(*p.ReportedAt) {
			continue
		}
		total += int64(d.SizeGib) * bytesPerGiB
	}
	return total, nil
}
