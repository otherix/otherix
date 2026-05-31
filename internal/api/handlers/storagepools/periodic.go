// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/queue"
	"github.com/otherix/otherix/internal/store"
)

// ScanTriggerStore is the storage surface the periodic scan trigger depends on:
// the pool list plus the atomic task+job enqueue. *etcdstore.Store satisfies it.
type ScanTriggerStore interface {
	ListPoolsNeedingScan(ctx context.Context) ([]store.ListPoolsNeedingScanRow, error)
	EnqueueTask(ctx context.Context, params store.CreateTaskParams, args queue.JobArgs) (uuid.UUID, error)
}

// scanTriggerMaxAttempts mirrors the river scan-task attempt budget.
const scanTriggerMaxAttempts = 25

// ScanTriggerFunc returns the periodic function that fans out a scan task for
// every pool needing one. The etcd-runtime replacement for the river
// ScanTriggerWorker; the Scheduler drives it. Unlike the river trigger it does
// not apply per-pool jitter (the etcd job has no scheduled-at), so all eligible
// pools enqueue at once - acceptable at MVP scale, revisitable if agent-side
// scan load needs spreading.
func ScanTriggerFunc(st ScanTriggerStore, log *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		pools, err := st.ListPoolsNeedingScan(ctx)
		if err != nil {
			return fmt.Errorf("list pools needing scan: %v", err)
		}
		var enqueued, failed int
		for _, p := range pools {
			taskID := uuid.New()
			poolID := p.ID
			if _, err := st.EnqueueTask(ctx, store.CreateTaskParams{
				ID:           taskID,
				Type:         "storage_pool.scan",
				Status:       store.TaskStatusPending,
				ResourceType: "storage_pool",
				ResourceID:   &poolID,
				Args:         []byte(`{}`),
				MaxAttempts:  scanTriggerMaxAttempts,
			}, StoragePoolScanArgs{TaskID: taskID, PoolID: p.ID}); err != nil {
				log.WarnContext(ctx, "storage_pool.scan_trigger enqueue failed", slog.String("pool_id", p.ID.String()), slog.Any("error", err))
				failed++
				continue
			}
			enqueued++
		}
		log.InfoContext(ctx, "storage_pool.scan_trigger tick", slog.Int("pools_eligible", len(pools)), slog.Int("enqueued", enqueued), slog.Int("failed", failed))
		return nil
	}
}
