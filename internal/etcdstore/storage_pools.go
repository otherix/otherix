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

// Storage pools are multi-instance (ADR 0022): the same name may exist once per
// node, so uniqueness is scoped (node_id, lower(name)) via a per-node guard,
// and a cluster-wide name index backs the name-aggregated lookups. Effective
// capacity subtracts pending vm_disk commits (disks on the pool created after
// the last scan). Storage images are content-addressable; the refcounted delete
// fires the agent file delete only for the last sibling sharing a checksum.

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

func storageImageKey(id uuid.UUID) string { return etcd.Key("storage_images", id.String()) }

func storageImagePoolIndexKey(poolID, id uuid.UUID) string {
	return etcd.Key("index", "storage_images", "pool", poolID.String(), id.String())
}

func storageImagePoolIndexPrefix(poolID uuid.UUID) string {
	return etcd.Key("index", "storage_images", "pool", poolID.String()) + "/"
}

func storageImageTemplateIndexKey(templateID, id uuid.UUID) string {
	return etcd.Key("index", "storage_images", "template", templateID.String(), id.String())
}

// storageImageTemplatePoolGuard enforces UNIQUE(template_id, pool_id) on
// storage_images (the SQL ON CONFLICT target the import projection upserts
// against): at most one image row per (template, pool). Written on insert,
// dropped on refcounted delete.
func storageImageTemplatePoolGuard(templateID, poolID uuid.UUID) string {
	return etcd.Key("uniq", "storage_images", "template_pool", templateID.String(), poolID.String())
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
// Returns store.ErrNotFound when missing, or *store.ResourceInUseError (keys
// "storage_images" / "vm_disks") when materialised images or active disks block
// the delete.
func (s *Store) DeleteStoragePool(ctx context.Context, id uuid.UUID) error {
	p, err := s.StoragePoolByID(ctx, id)
	if err != nil {
		return err
	}
	imageCount, err := s.countPrefix(ctx, storageImagePoolIndexPrefix(id))
	if err != nil {
		return err
	}
	diskCount, err := s.countPrefix(ctx, vmDisksPoolIndexPrefix(id))
	if err != nil {
		return err
	}
	if imageCount > 0 || diskCount > 0 {
		blocking := map[string]int64{}
		if imageCount > 0 {
			blocking["storage_images"] = imageCount
		}
		if diskCount > 0 {
			blocking["vm_disks"] = diskCount
		}
		return &store.ResourceInUseError{Resources: blocking}
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
		).
		Commit(); err != nil {
		return fmt.Errorf("delete storage pool txn: %v", err)
	}
	return nil
}

// StorageImageByID returns the storage image with the given id, or
// store.ErrNotFound.
func (s *Store) StorageImageByID(ctx context.Context, id uuid.UUID) (store.StorageImage, error) {
	var im store.StorageImage
	found, err := s.c.GetJSON(ctx, storageImageKey(id), &im)
	if err != nil {
		return store.StorageImage{}, err
	}
	if !found {
		return store.StorageImage{}, store.ErrNotFound
	}
	return im, nil
}

// ListStorageImagesByPool returns the images in a pool ordered by (imported_at,
// id) descending (newest first), after the cursor, capped at LimitCount.
func (s *Store) ListStorageImagesByPool(ctx context.Context, arg store.ListStorageImagesByPoolParams) ([]store.StorageImage, error) {
	images, err := s.imagesInPool(ctx, arg.PoolID)
	if err != nil {
		return nil, err
	}
	out := images[:0]
	for _, im := range images {
		if !beforeImageCursor(im, arg.CursorImportedAt, arg.CursorID) {
			continue
		}
		out = append(out, im)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ImportedAt.Equal(out[j].ImportedAt) {
			return out[i].ImportedAt.After(out[j].ImportedAt)
		}
		return out[i].ID.String() > out[j].ID.String()
	})
	if n := int(arg.LimitCount); n > 0 && len(out) > n {
		out = out[:n]
	}
	return out, nil
}

// DeleteStorageImageRefcounted removes an image row, invoking onLastReferent
// only when no sibling in the pool shares the checksum, so the on-disk file and
// the row are removed together. Returns store.ErrNotFound when the image is
// missing or addressed through the wrong pool. authorize gates the delete after
// the owning template loads; its error and onLastReferent's propagate verbatim.
func (s *Store) DeleteStorageImageRefcounted(
	ctx context.Context,
	poolID, imageID uuid.UUID,
	authorize func(store.Template) error,
	onLastReferent func(ctx context.Context, node store.Node, poolName, checksum string) error,
) (store.StorageImage, store.Template, error) {
	image, err := s.StorageImageByID(ctx, imageID)
	if err != nil {
		return store.StorageImage{}, store.Template{}, err
	}
	if image.PoolID != poolID {
		return store.StorageImage{}, store.Template{}, store.ErrNotFound
	}
	template, err := s.TemplateByID(ctx, image.TemplateID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.StorageImage{}, store.Template{}, fmt.Errorf("template %s missing for image %s", image.TemplateID, imageID)
		}
		return store.StorageImage{}, store.Template{}, err
	}
	if err := authorize(template); err != nil {
		return store.StorageImage{}, store.Template{}, err
	}

	siblings, err := s.countImageChecksumSiblings(ctx, poolID, image.ChecksumSha256, imageID)
	if err != nil {
		return store.StorageImage{}, store.Template{}, err
	}
	if siblings == 0 {
		pool, err := s.StoragePoolByID(ctx, poolID)
		if err != nil {
			return store.StorageImage{}, store.Template{}, fmt.Errorf("get pool for agent delete: %v", err)
		}
		node, err := s.NodeByID(ctx, pool.NodeID)
		if err != nil {
			return store.StorageImage{}, store.Template{}, fmt.Errorf("get node for agent delete: %v", err)
		}
		if err := onLastReferent(ctx, node, pool.Name, image.ChecksumSha256); err != nil {
			return store.StorageImage{}, store.Template{}, err
		}
	}

	if _, err := s.c.Raw().Txn(ctx).
		Then(
			clientv3.OpDelete(storageImageKey(imageID)),
			clientv3.OpDelete(storageImagePoolIndexKey(poolID, imageID)),
			clientv3.OpDelete(storageImageTemplateIndexKey(image.TemplateID, imageID)),
			clientv3.OpDelete(storageImageTemplatePoolGuard(image.TemplateID, poolID)),
		).
		Commit(); err != nil {
		return store.StorageImage{}, store.Template{}, fmt.Errorf("delete storage image txn: %v", err)
	}
	return image, template, nil
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

// imagesInPool loads every image in a pool via the pool index.
func (s *Store) imagesInPool(ctx context.Context, poolID uuid.UUID) ([]store.StorageImage, error) {
	items, err := s.c.Range(ctx, storageImagePoolIndexPrefix(poolID))
	if err != nil {
		return nil, err
	}
	out := make([]store.StorageImage, 0, len(items))
	for _, kv := range items {
		id, perr := uuid.Parse(string(kv.Value))
		if perr != nil {
			return nil, fmt.Errorf("corrupt image pool index %q: %v", kv.Key, perr)
		}
		im, err := s.StorageImageByID(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return nil, err
		}
		out = append(out, im)
	}
	return out, nil
}

// countImageChecksumSiblings counts images in the pool sharing the checksum,
// excluding excludeID.
func (s *Store) countImageChecksumSiblings(ctx context.Context, poolID uuid.UUID, checksum string, excludeID uuid.UUID) (int64, error) {
	images, err := s.imagesInPool(ctx, poolID)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, im := range images {
		if im.ID != excludeID && im.ChecksumSha256 == checksum {
			n++
		}
	}
	return n, nil
}

// beforeImageCursor reports whether (imported_at, id) sorts strictly before the
// DESC cursor tuple. A nil cursor (first page) admits every row.
func beforeImageCursor(im store.StorageImage, cursorImportedAt *time.Time, cursorID *uuid.UUID) bool {
	if cursorImportedAt == nil || cursorID == nil {
		return true
	}
	if !im.ImportedAt.Equal(*cursorImportedAt) {
		return im.ImportedAt.Before(*cursorImportedAt)
	}
	return im.ID.String() < cursorID.String()
}
