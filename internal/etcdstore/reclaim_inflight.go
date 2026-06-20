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

// reclaimInflight marks that a reclaim of one blob copy from one node is in
// flight, so the durability reconcile loop (which runs on every replica) does not
// re-enqueue a duplicate each pass while one is still running. It is a soft,
// self-expiring hint, NOT a durable lock: ExpiresAt bounds it so a worker that
// crashed without clearing the marker does not suppress re-reclaim forever, and
// two replicas racing may rarely both proceed (a duplicate reclaim is idempotent
// because the agent delete of an absent blob is a no-op).
type reclaimInflight struct {
	ExpiresAt time.Time `json:"expires_at"`
}

func reclaimInflightKey(digest string, nodeID uuid.UUID) string {
	return etcd.Key("index", "reclaim_inflight", digest, nodeID.String())
}

// TryBeginReclaim records an in-flight reclaim of digest from nodeID and reports
// whether the caller may proceed to enqueue the task. Returns false when a fresh
// (non-expired) marker already exists. A marker whose ExpiresAt has passed is
// treated as absent and overwritten. ttl bounds how long the marker suppresses
// re-enqueue if the worker never clears it.
func (s *Store) TryBeginReclaim(ctx context.Context, digest string, nodeID uuid.UUID, ttl time.Duration) (bool, error) {
	k := reclaimInflightKey(digest, nodeID)
	now := time.Now().UTC()
	enc, err := etcd.Marshal(reclaimInflight{ExpiresAt: now.Add(ttl)})
	if err != nil {
		return false, err
	}
	// Fast path: atomically create the marker when absent. Only one racing replica
	// wins the create, so the common first-time double-enqueue is impossible.
	created, err := s.c.PutIfAbsent(ctx, k, enc)
	if err != nil {
		return false, fmt.Errorf("create reclaim marker: %v", err)
	}
	if created {
		return true, nil
	}
	// A marker already exists. If it is still fresh, back off; if it has expired,
	// overwrite it (the loser of an expired-marker race re-creates an idempotent
	// reclaim of the same HRW-deterministic victim, so a rare double here is safe).
	val, found, err := s.c.Get(ctx, k)
	if err != nil {
		return false, fmt.Errorf("read reclaim marker: %v", err)
	}
	if found {
		var cur reclaimInflight
		if json.Unmarshal(val, &cur) == nil && cur.ExpiresAt.After(now) {
			return false, nil
		}
	}
	if err := s.c.Put(ctx, k, enc); err != nil {
		return false, fmt.Errorf("refresh reclaim marker: %v", err)
	}
	return true, nil
}

// EndReclaim clears the in-flight marker for (digest, nodeID) so the reconcile
// loop may re-evaluate immediately. Called by the reclaim worker on finalize
// (success or terminal failure). A missing marker is not an error.
func (s *Store) EndReclaim(ctx context.Context, digest string, nodeID uuid.UUID) error {
	if _, err := s.c.Delete(ctx, reclaimInflightKey(digest, nodeID)); err != nil {
		return fmt.Errorf("clear reclaim marker: %v", err)
	}
	return nil
}
