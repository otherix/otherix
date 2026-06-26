// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package nodes

import (
	"context"
	"log/slog"
)

// DrainReconcileStore is the store surface the drain backstop needs.
type DrainReconcileStore interface {
	ReconcileStuckDrain(ctx context.Context) (int, error)
}

// DrainReconcileFunc returns a periodic that un-wedges drain-stuck nodes: nodes
// left in draining whose drain task is missing or terminal are finalized to
// cordoned. A node whose drain task is still in flight is left alone.
func DrainReconcileFunc(st DrainReconcileStore, log *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		n, err := st.ReconcileStuckDrain(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			log.InfoContext(ctx, "drain backstop cordoned wedged nodes", "count", n)
		}
		return nil
	}
}
