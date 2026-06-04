// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package networks

import (
	"context"
	"fmt"
	"log/slog"
)

// CleanupStore is the storage surface the orphaned-status sweep depends on.
// *etcdstore.Store satisfies it.
type CleanupStore interface {
	DeleteOrphanedNetworkNodeStatus(ctx context.Context) (int64, error)
}

// CleanupFunc returns the periodic function that deletes network_node_status
// records whose network no longer resolves to a live row. It backstops the
// best-effort purge in DeleteNetwork, which nothing else re-drives; the
// Scheduler drives this sweep.
func CleanupFunc(st CleanupStore, log *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		deleted, err := st.DeleteOrphanedNetworkNodeStatus(ctx)
		if err != nil {
			return fmt.Errorf("delete orphaned network_node_status: %v", err)
		}
		if deleted > 0 {
			log.InfoContext(ctx, "networks.cleanup", "orphaned_node_status_deleted", deleted)
		}
		return nil
	}
}
