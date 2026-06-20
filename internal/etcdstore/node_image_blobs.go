// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// image_blobs/<node> holds the observed per-node inventory of the pinned-image
// cache tier. It is a SEPARATE family from node_blobs: the durability reconcile
// and the reference-counted reclaim read only node_blobs and placement, so a
// cached image is never given a placement key and never reclaimed. There is
// deliberately no AllNodeImageBlobDigests scan - nothing must fold this family
// into the reconcile.

func nodeImageBlobInventoryKey(nodeID uuid.UUID) string {
	return etcd.Key("image_blobs", nodeID.String())
}

func nodeImageBlobInventoryPrefix() string {
	return etcd.Key("image_blobs") + "/"
}

// UpsertNodeImageBlobInventory replaces the node's reported image-cache
// inventory. An empty list deletes the key (the node holds no cached images).
func (s *Store) UpsertNodeImageBlobInventory(ctx context.Context, nodeID uuid.UUID, blobs []store.NodeBlob) error {
	if len(blobs) == 0 {
		_, err := s.c.Delete(ctx, nodeImageBlobInventoryKey(nodeID))
		return err
	}
	return s.c.PutJSON(ctx, nodeImageBlobInventoryKey(nodeID), blobs)
}

// ImageBlobHolders returns the node ids whose reported image-cache inventory
// contains digest, sorted for determinism.
func (s *Store) ImageBlobHolders(ctx context.Context, digest string) ([]uuid.UUID, error) {
	items, err := s.c.Range(ctx, nodeImageBlobInventoryPrefix())
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
		if !s.decodeOrQuarantine(ctx, kv.Key, kv.Value, &blobs, "node_image_blob") {
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
