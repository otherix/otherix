// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/otherix/otherix/internal/store"
)

// ReconcileStore is the storage surface the node-health reconcile depends on.
// *etcdstore.Store satisfies it.
type ReconcileStore interface {
	PromoteHealthyNodes(ctx context.Context, freshAfter time.Time) ([]store.PromoteHealthyNodesRow, error)
	MarkNodesUnreachable(ctx context.Context, staleBefore time.Time) ([]store.MarkNodesUnreachableRow, error)
}

// ReconcileFunc returns the periodic function that flips nodes between 'ready'
// and 'unreachable' on heartbeat freshness. The etcd-runtime replacement for the
// river ReconcileWorker; the Scheduler drives it (run-on-start).
func ReconcileFunc(st ReconcileStore, cfg ReconcileConfig, log *slog.Logger) func(context.Context) error {
	c := cfg.withDefaults()
	return func(ctx context.Context) error {
		freshAfter := time.Now().Add(-c.StaleThreshold)
		promoted, err := st.PromoteHealthyNodes(ctx, freshAfter)
		if err != nil {
			return fmt.Errorf("promote healthy nodes: %v", err)
		}
		for _, row := range promoted {
			log.InfoContext(ctx, "node promoted to ready", slog.String("node_id", row.ID.String()), slog.String("node_name", row.Name))
		}
		demoted, err := st.MarkNodesUnreachable(ctx, freshAfter)
		if err != nil {
			return fmt.Errorf("mark nodes unreachable: %v", err)
		}
		for _, row := range demoted {
			log.WarnContext(ctx, "node marked unreachable", slog.String("node_id", row.ID.String()), slog.String("node_name", row.Name))
		}
		return nil
	}
}
