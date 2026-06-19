// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package replication

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// GoneBlobStore is the surface the gone-node blob-inventory prune needs.
// *etcdstore.Store satisfies it (UpsertNodeBlobInventory with an empty slice
// deletes the node's inventory key).
type GoneBlobStore interface {
	UpsertNodeBlobInventory(ctx context.Context, nodeID uuid.UUID, blobs []store.NodeBlob) error
}

// PruneGoneNodeBlobsFunc returns a node-gone hook that drops the observed blob
// inventory of each node that reached terminal 'gone', so a dead node stops
// counting as an observed blob holder. Best-effort: an error is logged and the
// sweep continues (the inventory is observed state, not durable intent).
func PruneGoneNodeBlobsFunc(st GoneBlobStore, log *slog.Logger) func(context.Context, []store.MarkNodesGoneRow) error {
	return func(ctx context.Context, gone []store.MarkNodesGoneRow) error {
		for _, n := range gone {
			if err := st.UpsertNodeBlobInventory(ctx, n.ID, nil); err != nil {
				log.WarnContext(ctx, "prune gone node blob inventory failed",
					slog.String("node_id", n.ID.String()), slog.Any("error", err))
			}
		}
		return nil
	}
}
