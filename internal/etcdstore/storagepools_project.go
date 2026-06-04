// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// ProjectStorageImageImport projects an import result into the storage_images
// row, back-propagates the agent-computed checksum onto a compute-mode template,
// and finalizes the import task - all in one transaction. The storage_images
// write upserts on the (template_id, pool_id) guard (the SQL ON CONFLICT target):
// an existing row keeps its id + imported_at and refreshes checksum / size /
// format; a fresh row is inserted with its pool + template indexes and the
// guard. The whole projection commits under a compare on the template
// mod-revision (so the if-null checksum write never clobbers a concurrent
// derived_vm_count change) plus, on the insert path, a create-revision compare
// on the guard (so a concurrent insert loses and retries onto the upsert path).
func (s *Store) ProjectStorageImageImport(ctx context.Context, img store.CreateStorageImageParams, tmplChecksum store.UpdateTemplateImageChecksumIfNullParams, fin store.UpdateTaskFinalizedParams) error {
	taskVal, err := s.finalizedTaskValue(ctx, fin)
	if err != nil {
		return err
	}
	return s.projectStorageImageImport(ctx, img, tmplChecksum, clientv3.OpPut(taskKey(fin.ID), string(taskVal)))
}

// EnsureStorageImageProjected runs the same storage_images upsert and
// compute-mode template checksum back-propagation as ProjectStorageImageImport
// but finalizes no task. The inline VM-create path that materializes an image on
// demand calls this: there is no separate import task to finalize, so the shared
// CAS body commits with no extra ops.
func (s *Store) EnsureStorageImageProjected(ctx context.Context, img store.CreateStorageImageParams, tmplChecksum store.UpdateTemplateImageChecksumIfNullParams) error {
	return s.projectStorageImageImport(ctx, img, tmplChecksum)
}

// projectStorageImageImport is the shared CAS body behind
// ProjectStorageImageImport and EnsureStorageImageProjected. It upserts the
// storage_images row on the (template_id, pool_id) guard, back-propagates the
// checksum onto a compute-mode template, and appends extraOps (e.g. a finalized
// task put) to the committed transaction. See ProjectStorageImageImport for the
// compare semantics.
func (s *Store) projectStorageImageImport(ctx context.Context, img store.CreateStorageImageParams, tmplChecksum store.UpdateTemplateImageChecksumIfNullParams, extraOps ...clientv3.Op) error {
	now := time.Now().UTC()
	guard := storageImageTemplatePoolGuard(img.TemplateID, img.PoolID)

	for range projectTemplateCASRetries {
		template, tmplRev, found, err := s.templateWithRev(ctx, img.TemplateID)
		if err != nil {
			return err
		}
		if !found {
			return store.ErrNotFound
		}

		// Back-propagate the checksum only when the template has none yet.
		var tmplOps []clientv3.Op
		if len(template.ImageChecksumSha256) == 0 && len(tmplChecksum.ImageChecksumSha256) > 0 {
			template.ImageChecksumSha256 = tmplChecksum.ImageChecksumSha256
			tval, merr := etcd.Marshal(template)
			if merr != nil {
				return merr
			}
			tmplOps = []clientv3.Op{clientv3.OpPut(templateKey(img.TemplateID), string(tval))}
		}

		existingID, guardFound, err := s.resolveGuard(ctx, guard)
		if err != nil {
			return err
		}

		cmps := []clientv3.Cmp{clientv3.Compare(clientv3.ModRevision(templateKey(img.TemplateID)), "=", tmplRev)}
		var ops []clientv3.Op
		if guardFound {
			image, err := s.StorageImageByID(ctx, existingID)
			if err != nil {
				return err
			}
			image.ChecksumSha256 = img.ChecksumSha256
			image.SizeBytes = img.SizeBytes
			image.Format = img.Format
			ival, merr := etcd.Marshal(image)
			if merr != nil {
				return merr
			}
			ops = append(ops, clientv3.OpPut(storageImageKey(existingID), string(ival)))
		} else {
			image := store.StorageImage{
				ID:             img.ID,
				TemplateID:     img.TemplateID,
				PoolID:         img.PoolID,
				ChecksumSha256: img.ChecksumSha256,
				SizeBytes:      img.SizeBytes,
				Format:         img.Format,
				ImportedAt:     now,
			}
			ival, merr := etcd.Marshal(image)
			if merr != nil {
				return merr
			}
			cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(guard), "=", 0))
			ops = append(ops,
				clientv3.OpPut(storageImageKey(img.ID), string(ival)),
				clientv3.OpPut(storageImagePoolIndexKey(img.PoolID, img.ID), img.ID.String()),
				clientv3.OpPut(storageImageTemplateIndexKey(img.TemplateID, img.ID), img.ID.String()),
				clientv3.OpPut(guard, img.ID.String()),
			)
		}
		ops = append(ops, tmplOps...)
		ops = append(ops, extraOps...)

		resp, err := s.c.Raw().Txn(ctx).If(cmps...).Then(ops...).Commit()
		if err != nil {
			return fmt.Errorf("project storage image import txn: %v", err)
		}
		if resp.Succeeded {
			return nil
		}
	}
	return fmt.Errorf("project storage image import: CAS exhausted after %d tries", projectTemplateCASRetries)
}

// Storage-pool worker projections. The scan worker merges the agent-reported
// usage and the recomputed disk-pressure state onto the pool row and finalizes
// the scan task in one transaction. Every write is idempotent (a re-run sets the
// same values), so the transaction only groups them; the pure pressure
// transition is computed by the Run-form worker and passed in already resolved.

// ProjectStoragePoolScan merges the scan result (capacity / available, stamping
// reported_at) and the recomputed disk-pressure state onto the pool row, then
// finalizes the scan task - all in one transaction. A soft-deleted pool skips
// the pool write (matching the SQL `where deleted_at is null` no-op) but the
// task is still finalized.
func (s *Store) ProjectStoragePoolScan(ctx context.Context, usage store.UpsertStoragePoolUsageParams, pressure store.UpdatePoolDiskPressureParams, fin store.UpdateTaskFinalizedParams) error {
	now := time.Now().UTC()
	taskVal, err := s.finalizedTaskValue(ctx, fin)
	if err != nil {
		return err
	}

	var ops []clientv3.Op
	pool, err := s.StoragePoolByID(ctx, usage.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Pool deleted between enqueue and projection: skip the usage write.
	case err != nil:
		return err
	default:
		pool.CapacityBytes = usage.CapacityBytes
		pool.AvailableBytes = usage.AvailableBytes
		pool.ReportedAt = &now
		pool.DiskPressureSince = pressure.DiskPressureSince
		pool.DiskPressureCount = pressure.DiskPressureCount
		pool.UpdatedAt = now
		val, merr := etcd.Marshal(pool)
		if merr != nil {
			return merr
		}
		ops = append(ops, clientv3.OpPut(storagePoolKey(usage.ID), string(val)))
	}

	ops = append(ops, clientv3.OpPut(taskKey(fin.ID), string(taskVal)))
	if _, err := s.c.Raw().Txn(ctx).Then(ops...).Commit(); err != nil {
		return fmt.Errorf("project storage pool scan txn: %v", err)
	}
	return nil
}
