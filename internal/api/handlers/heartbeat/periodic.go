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
	RewriteGoneNodesToUnreachable(ctx context.Context) ([]store.MarkNodesUnreachableRow, error)
}

// NodeReadyHook is the seam invoked once per reconcile pass with the set of
// nodes that just transitioned to 'ready' (the PromoteHealthyNodes result). It
// is best-effort: ReconcileFunc logs and swallows any error so node-health
// reconcile keeps running. Default-pool provisioning is the first consumer.
type NodeReadyHook func(ctx context.Context, ready []store.PromoteHealthyNodesRow) error

// ReconcileFunc returns the periodic function that flips nodes between 'ready'
// and 'unreachable' on heartbeat freshness. The etcd-runtime periodic
// reconcile worker; the Scheduler drives it (run-on-start).
//
// onReady is the post-promotion seam (may be nil). It fires only when at least
// one node was promoted; its error is logged at WARN and otherwise ignored - a
// provisioning hiccup must NOT abort the pass, or MarkNodesUnreachable would stop
// running. The work retries on the next promotion.
func ReconcileFunc(st ReconcileStore, cfg ReconcileConfig, log *slog.Logger, onReady NodeReadyHook) func(context.Context) error {
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
		if onReady != nil && len(promoted) > 0 {
			if err := onReady(ctx, promoted); err != nil {
				log.WarnContext(ctx, "node-ready hook failed; continuing reconcile", slog.String("error", err.Error()))
			}
		}
		demoted, err := st.MarkNodesUnreachable(ctx, freshAfter)
		if err != nil {
			return fmt.Errorf("mark nodes unreachable: %v", err)
		}
		for _, row := range demoted {
			log.WarnContext(ctx, "node marked unreachable", slog.String("node_id", row.ID.String()), slog.String("node_name", row.Name))
		}
		// Transition-release sweep: rewrite any pre-existing 'gone' row to the
		// recoverable 'unreachable' so a self-healing node is promoted again.
		rewritten, err := st.RewriteGoneNodesToUnreachable(ctx)
		if err != nil {
			return fmt.Errorf("rewrite gone nodes: %v", err)
		}
		for _, row := range rewritten {
			log.InfoContext(ctx, "rewrote retired gone node to unreachable", slog.String("node_id", row.ID.String()), slog.String("node_name", row.Name))
		}
		return nil
	}
}
