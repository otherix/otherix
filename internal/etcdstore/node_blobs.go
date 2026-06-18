// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// node_blobs/<nodeID> -> JSON []store.NodeBlob. Observed state, heartbeat-fed
// (last-writer-wins, no guard), mirroring pool_images/<poolID>. The holder-
// discovery source for the cross-node pull saga (slice C1). NOT the placement
// map (placement/<digest>/<node>): observed inventory is ephemeral truth,
// placement is durable intent; C1 keeps them separate.
func nodeBlobInventoryKey(nodeID uuid.UUID) string {
	return etcd.Key("node_blobs", nodeID.String())
}

func nodeBlobInventoryPrefix() string {
	return etcd.Key("node_blobs") + "/"
}

// UpsertNodeBlobInventory replaces the observed blob inventory for a node with
// blobs. A blind put, last-writer-wins per heartbeat. An empty slice clears the
// inventory. The fail-closed-preserve-on-unavailable discipline lives in the
// heartbeat handler (it skips the call entirely on BlobsUnavailable), NOT here.
func (s *Store) UpsertNodeBlobInventory(ctx context.Context, nodeID uuid.UUID, blobs []store.NodeBlob) error {
	if len(blobs) == 0 {
		_, err := s.c.Delete(ctx, nodeBlobInventoryKey(nodeID))
		return err
	}
	return s.c.PutJSON(ctx, nodeBlobInventoryKey(nodeID), blobs)
}

// NodeBlobInventory returns the observed blob inventory for a node, or an empty
// slice when none was reported yet.
func (s *Store) NodeBlobInventory(ctx context.Context, nodeID uuid.UUID) ([]store.NodeBlob, error) {
	var out []store.NodeBlob
	found, err := s.c.GetJSON(ctx, nodeBlobInventoryKey(nodeID), &out)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return out, nil
}

// BlobHolders scans every node's observed blob inventory and returns the node
// ids that report holding digest right now (observed presence, NOT the durable
// placement map). The holder-discovery step of the pull saga. Ordered by node
// id for deterministic holder selection. An empty result means no live holder
// (the saga then fails fail-closed blob_unavailable).
func (s *Store) BlobHolders(ctx context.Context, digest string) ([]uuid.UUID, error) {
	items, err := s.c.Range(ctx, nodeBlobInventoryPrefix())
	if err != nil {
		return nil, err
	}
	var holders []uuid.UUID
	for _, kv := range items {
		nodeID, perr := uuid.Parse(kv.Key[strings.LastIndexByte(kv.Key, '/')+1:])
		if perr != nil {
			continue
		}
		var blobs []store.NodeBlob
		if uerr := json.Unmarshal(kv.Value, &blobs); uerr != nil {
			continue
		}
		for _, b := range blobs {
			if b.Digest == digest {
				holders = append(holders, nodeID)
				break
			}
		}
	}
	sort.Slice(holders, func(i, j int) bool { return holders[i].String() < holders[j].String() })
	return holders, nil
}
