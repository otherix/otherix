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
	MarkNodesGone(ctx context.Context, goneBefore time.Time) ([]store.MarkNodesGoneRow, error)
}

// NodeReadyHook is the seam invoked once per reconcile pass with the set of
// nodes that just transitioned to 'ready' (the PromoteHealthyNodes result). It
// is best-effort: ReconcileFunc logs and swallows any error so node-health
// reconcile keeps running. Default-pool provisioning is the first consumer.
type NodeReadyHook func(ctx context.Context, ready []store.PromoteHealthyNodesRow) error

// NodeGoneHook is the seam invoked once per reconcile pass with the set of nodes
// that just transitioned to terminal 'gone'. Best-effort: ReconcileFunc logs and
// swallows any error so node-health reconcile keeps running. Pruning a gone node's
// observed blob inventory is the first consumer.
type NodeGoneHook func(ctx context.Context, gone []store.MarkNodesGoneRow) error

// ReconcileFunc returns the periodic function that flips nodes between 'ready'
// and 'unreachable' on heartbeat freshness. The etcd-runtime replacement for the
// river ReconcileWorker; the Scheduler drives it (run-on-start).
//
// onReady is the post-promotion seam (may be nil). It fires only when at least
// one node was promoted; its error is logged at WARN and otherwise ignored - a
// provisioning hiccup must NOT abort the pass, or MarkNodesUnreachable /
// MarkNodesGone would stop running. The work retries on the next promotion.
func ReconcileFunc(st ReconcileStore, cfg ReconcileConfig, log *slog.Logger, onReady NodeReadyHook, onGone NodeGoneHook) func(context.Context) error {
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
		goneBefore := time.Now().Add(-c.GoneGrace)
		gone, err := st.MarkNodesGone(ctx, goneBefore)
		if err != nil {
			return fmt.Errorf("mark nodes gone: %v", err)
		}
		for _, row := range gone {
			log.WarnContext(ctx, "node marked gone", slog.String("node_id", row.ID.String()), slog.String("node_name", row.Name))
		}
		if onGone != nil && len(gone) > 0 {
			if err := onGone(ctx, gone); err != nil {
				log.WarnContext(ctx, "node-gone hook failed; continuing reconcile", slog.String("error", err.Error()))
			}
		}
		return nil
	}
}
