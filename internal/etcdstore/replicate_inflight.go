// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
)

// replicateInflight marks that a replication of one blob to one node is in
// flight, so the durability reconcile loop (which runs on every replica) does not
// re-enqueue a duplicate each pass while a pull is still running. It is a soft,
// self-expiring hint, NOT a durable lock: ExpiresAt bounds it so a worker that
// crashed without clearing the marker does not suppress re-replication forever,
// and two replicas racing may rarely both proceed (a duplicate pull is idempotent
// because blobs are content-addressed).
type replicateInflight struct {
	ExpiresAt time.Time `json:"expires_at"`
}

func replicateInflightKey(digest string, nodeID uuid.UUID) string {
	return etcd.Key("index", "replicate_inflight", digest, nodeID.String())
}

// TryBeginReplicate records an in-flight replication of digest to nodeID and
// reports whether the caller may proceed to enqueue the task. Returns false when
// a fresh (non-expired) marker already exists. A marker whose ExpiresAt has
// passed is treated as absent and overwritten. ttl bounds how long the marker
// suppresses re-enqueue if the worker never clears it.
func (s *Store) TryBeginReplicate(ctx context.Context, digest string, nodeID uuid.UUID, ttl time.Duration) (bool, error) {
	k := replicateInflightKey(digest, nodeID)
	now := time.Now().UTC()
	enc, err := etcd.Marshal(replicateInflight{ExpiresAt: now.Add(ttl)})
	if err != nil {
		return false, err
	}
	// Fast path: atomically create the marker when absent. Only one racing replica
	// wins the create, so the common first-time double-enqueue is impossible.
	created, err := s.c.PutIfAbsent(ctx, k, enc)
	if err != nil {
		return false, fmt.Errorf("create replicate marker: %v", err)
	}
	if created {
		return true, nil
	}
	// A marker already exists. If it is still fresh, back off; if it has expired,
	// overwrite it (the loser of an expired-marker race re-creates an idempotent
	// content-addressed pull, so a rare double here is safe).
	val, found, err := s.c.Get(ctx, k)
	if err != nil {
		return false, fmt.Errorf("read replicate marker: %v", err)
	}
	if found {
		var cur replicateInflight
		if json.Unmarshal(val, &cur) == nil && cur.ExpiresAt.After(now) {
			return false, nil
		}
	}
	if err := s.c.Put(ctx, k, enc); err != nil {
		return false, fmt.Errorf("refresh replicate marker: %v", err)
	}
	return true, nil
}

// EndReplicate clears the in-flight marker for (digest, nodeID) so the reconcile
// loop may re-evaluate immediately. Called by the replicate worker on finalize
// (success or terminal failure). A missing marker is not an error.
func (s *Store) EndReplicate(ctx context.Context, digest string, nodeID uuid.UUID) error {
	if _, err := s.c.Delete(ctx, replicateInflightKey(digest, nodeID)); err != nil {
		return fmt.Errorf("clear replicate marker: %v", err)
	}
	return nil
}
