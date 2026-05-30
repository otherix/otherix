// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The methods in this file are the storage-pool and storage-image
// domain entrypoints on *Store: they wrap the raw sqlc-generated
// *Queries primitives, translate driver errors into store sentinels,
// and assemble the transactional deletes. Handlers depend on these, not
// on *Queries.

// ErrStoragePoolNameExists is returned when a storage pool with the
// same name already exists on the target node (uq_storage_pools_name is
// scoped per node).
var ErrStoragePoolNameExists = errors.New("store: storage pool name already in use on node")

// translateStoragePoolUnique maps the per-node name-uniqueness
// violation to its domain sentinel; any other error passes through.
func translateStoragePoolUnique(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrStoragePoolNameExists
	}
	return err
}

// CreateStoragePool inserts a storage pool, translating the per-node
// name-uniqueness violation to ErrStoragePoolNameExists.
func (s *Store) CreateStoragePool(ctx context.Context, arg CreateStoragePoolParams) (StoragePool, error) {
	p, err := s.queries.CreateStoragePool(ctx, arg)
	if err != nil {
		return StoragePool{}, translateStoragePoolUnique(err)
	}
	return p, nil
}

// UpdateStoragePool updates a storage pool, translating the per-node
// name-uniqueness violation to ErrStoragePoolNameExists.
func (s *Store) UpdateStoragePool(ctx context.Context, arg UpdateStoragePoolParams) (StoragePool, error) {
	p, err := s.queries.UpdateStoragePool(ctx, arg)
	if err != nil {
		return StoragePool{}, translateStoragePoolUnique(err)
	}
	return p, nil
}

// PoolEffectiveByID returns the pool joined with its effective-capacity
// view, or ErrNotFound.
func (s *Store) PoolEffectiveByID(ctx context.Context, id uuid.UUID) (PoolEffectiveCapacity, error) {
	p, err := s.queries.GetPoolEffectiveByID(ctx, id)
	if err != nil {
		return PoolEffectiveCapacity{}, translateNoRows(err)
	}
	return p, nil
}

// ListPoolsEffective returns pools matching the cursor-paginated
// filters, each joined with its effective-capacity view.
func (s *Store) ListPoolsEffective(ctx context.Context, arg ListPoolsEffectiveParams) ([]PoolEffectiveCapacity, error) {
	return s.queries.ListPoolsEffective(ctx, arg)
}

// ListPoolsEffectiveByName returns every per-node instance sharing the
// given pool name, joined with the effective-capacity view.
func (s *Store) ListPoolsEffectiveByName(ctx context.Context, name string) ([]PoolEffectiveCapacity, error) {
	return s.queries.ListPoolsEffectiveByName(ctx, name)
}

// DeleteStoragePool soft-deletes a pool in a single transaction after
// verifying nothing still references it. Returns *ResourceInUseError
// (keys "storage_images" and/or "vm_disks") when materialised images or
// active vm disks block the delete.
func (s *Store) DeleteStoragePool(ctx context.Context, id uuid.UUID) error {
	return s.InTx(ctx, func(q *Queries) error {
		diskCount, err := q.CountVMDisksOnStoragePool(ctx, id)
		if err != nil {
			return err
		}
		imageCount, err := q.CountStorageImagesByPool(ctx, id)
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
			return &ResourceInUseError{Resources: blocking}
		}
		return q.SoftDeleteStoragePool(ctx, id)
	})
}

// StorageImageByID returns the storage image with the given id, or
// ErrNotFound.
func (s *Store) StorageImageByID(ctx context.Context, id uuid.UUID) (StorageImage, error) {
	im, err := s.queries.GetStorageImageByID(ctx, id)
	if err != nil {
		return StorageImage{}, translateNoRows(err)
	}
	return im, nil
}

// ListStorageImagesByPool returns the storage images in a pool,
// cursor-paginated.
func (s *Store) ListStorageImagesByPool(ctx context.Context, arg ListStorageImagesByPoolParams) ([]StorageImage, error) {
	return s.queries.ListStorageImagesByPool(ctx, arg)
}

// DeleteStorageImageRefcounted removes a storage image row in one
// transaction, invoking onLastReferent only when no sibling image in
// the same pool shares the checksum (so the on-disk file and the row
// commit or unwind together). It returns ErrNotFound when the image is
// missing or addressed through the wrong pool. authorize gates the
// delete after the owning template is loaded; its error (and
// onLastReferent's) propagates verbatim so callers can map their own
// sentinels. The returned image and template back the response view.
//
// The two callbacks keep policy (ownership) and the external side
// effect (the agent's file delete) in the handler while this method
// owns the *Queries orchestration: the handler never sees a pgx.Tx or
// *Queries.
func (s *Store) DeleteStorageImageRefcounted(
	ctx context.Context,
	poolID, imageID uuid.UUID,
	authorize func(Template) error,
	onLastReferent func(ctx context.Context, node Node, poolName, checksum string) error,
) (StorageImage, Template, error) {
	var (
		outImage    StorageImage
		outTemplate Template
	)
	err := s.InTx(ctx, func(q *Queries) error {
		image, err := q.GetStorageImageByID(ctx, imageID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("get storage image: %w", err)
		}
		if image.PoolID != poolID {
			return ErrNotFound
		}

		template, err := q.GetTemplate(ctx, image.TemplateID)
		if err != nil {
			// FK-guaranteed; missing here means a soft-deleted template,
			// a broken data-model invariant the caller surfaces as 500.
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("template %s missing for image %s", image.TemplateID, imageID)
			}
			return fmt.Errorf("get template: %w", err)
		}
		if err := authorize(template); err != nil {
			return err
		}

		count, err := q.CountStorageImagesBySHA256InPool(ctx, CountStorageImagesBySHA256InPoolParams{
			PoolID:         poolID,
			ChecksumSha256: image.ChecksumSha256,
			ExcludeID:      imageID,
		})
		if err != nil {
			return fmt.Errorf("count siblings: %w", err)
		}
		if count == 0 {
			pool, err := q.GetStoragePoolByID(ctx, poolID)
			if err != nil {
				return fmt.Errorf("get pool for agent delete: %w", err)
			}
			node, err := q.GetNodeByID(ctx, pool.NodeID)
			if err != nil {
				return fmt.Errorf("get node for agent delete: %w", err)
			}
			if err := onLastReferent(ctx, node, pool.Name, image.ChecksumSha256); err != nil {
				return err
			}
		}

		if err := q.DeleteStorageImage(ctx, imageID); err != nil {
			return fmt.Errorf("delete storage image row: %w", err)
		}
		outImage, outTemplate = image, template
		return nil
	})
	if err != nil {
		return StorageImage{}, Template{}, err
	}
	return outImage, outTemplate, nil
}
