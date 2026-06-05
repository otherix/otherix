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
