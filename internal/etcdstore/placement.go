// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
)

// placementRootPrefix ranges every placement entry across all digests. The
// per-digest scan uses placementPrefix(digest); this is the cluster-wide sweep
// the durability reconcile loop walks to find every produced blob.
func placementRootPrefix() string { return etcd.Key("placement") + "/" }

// AllPlacementDigests returns every digest that has at least one placement entry
// (the set of produced blobs the durability reconcile loop must hold to K).
// Order unspecified; duplicates collapsed.
func (s *Store) AllPlacementDigests(ctx context.Context) ([]string, error) {
	items, err := s.c.Range(ctx, placementRootPrefix())
	if err != nil {
		return nil, fmt.Errorf("range placement: %v", err)
	}
	seen := make(map[string]struct{}, len(items))
	out := make([]string, 0, len(items))
	for _, kv := range items {
		rest := strings.TrimPrefix(kv.Key, placementRootPrefix())
		slash := strings.IndexByte(rest, '/')
		if slash <= 0 {
			continue
		}
		digest := rest[:slash]
		if _, ok := seen[digest]; ok {
			continue
		}
		seen[digest] = struct{}{}
		out = append(out, digest)
	}
	return out, nil
}

// AddBlobPlacement records nodeID as a chosen durable holder of digest. The value
// mirrors the seed written by SnapshotManifestApplied (the bare node id), so
// BlobPlacements reads it unchanged. Idempotent: re-adding overwrites.
func (s *Store) AddBlobPlacement(ctx context.Context, digest string, nodeID uuid.UUID) error {
	if err := s.c.Put(ctx, placementKey(digest, nodeID), []byte(nodeID.String())); err != nil {
		return fmt.Errorf("add placement %q/%s: %v", digest, nodeID, err)
	}
	return nil
}

// RemoveBlobPlacement drops a chosen durable holder of digest. removed reports
// whether an entry was present. The durability reconcile loop calls it only to
// prune a holder that reached terminal 'gone', and never the last live pointer.
func (s *Store) RemoveBlobPlacement(ctx context.Context, digest string, nodeID uuid.UUID) (bool, error) {
	removed, err := s.c.Delete(ctx, placementKey(digest, nodeID))
	if err != nil {
		return false, fmt.Errorf("remove placement %q/%s: %v", digest, nodeID, err)
	}
	return removed, nil
}

// SnapshotsReferencingBlob returns the ids of snapshots whose reference-graph
// entries point at digest. The reverse of the refgraph write in
// SnapshotManifestApplied and the input to the per-blob desired-K computation.
// Order unspecified.
func (s *Store) SnapshotsReferencingBlob(ctx context.Context, digest string) ([]uuid.UUID, error) {
	items, err := s.c.Range(ctx, blobRefPrefix(digest))
	if err != nil {
		return nil, fmt.Errorf("range blob refs %q: %v", digest, err)
	}
	out := make([]uuid.UUID, 0, len(items))
	for _, kv := range items {
		id, perr := uuid.Parse(kv.Key[strings.LastIndexByte(kv.Key, '/')+1:])
		if perr != nil {
			return nil, fmt.Errorf("corrupt blob ref key %q: %v", kv.Key, perr)
		}
		out = append(out, id)
	}
	return out, nil
}
