// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// lb_published_listener_status/<lbID>/<nodeID> -> JSON
// store.LBPublishedListenerStatus. Observed state, heartbeat-fed (last-writer-wins
// per (lbID, nodeID) pair, no guard), read by the lb get view to surface a
// gateway that cannot bind the published port. The per-lb prefix range gives the
// view every gateway's listener status in one round trip; DeleteLoadBalancer
// cascades the whole <lbID>/ prefix.
func lbPublishedListenerStatusKey(lbID, nodeID uuid.UUID) string {
	return etcd.Key("lb_published_listener_status", lbID.String(), nodeID.String())
}

func lbPublishedListenerStatusPrefix(lbID uuid.UUID) string {
	return etcd.Key("lb_published_listener_status", lbID.String()) + "/"
}

// UpsertLBPublishedListenerStatus writes the observed listener bind state for
// (lbID, nodeID). A blind put, last-writer-wins per heartbeat.
func (s *Store) UpsertLBPublishedListenerStatus(ctx context.Context, lbID, nodeID uuid.UUID, port int32, bound bool, errMsg string, reportedAt time.Time) error {
	return s.c.PutJSON(ctx, lbPublishedListenerStatusKey(lbID, nodeID), store.LBPublishedListenerStatus{
		NodeID:     nodeID,
		Port:       port,
		Bound:      bound,
		Error:      errMsg,
		ReportedAt: reportedAt.UTC(),
	})
}

// ListLBPublishedListenerStatus returns every observed published-listener status
// record for a load balancer, one per reporting gateway node. An empty slice
// means nothing reported yet.
func (s *Store) ListLBPublishedListenerStatus(ctx context.Context, lbID uuid.UUID) ([]store.LBPublishedListenerStatus, error) {
	items, err := s.c.Range(ctx, lbPublishedListenerStatusPrefix(lbID))
	if err != nil {
		return nil, err
	}
	out := make([]store.LBPublishedListenerStatus, 0, len(items))
	for _, kv := range items {
		var st store.LBPublishedListenerStatus
		if !s.decodeOrQuarantine(ctx, kv.Key, kv.Value, &st, "lb_published_listener_status") {
			continue
		}
		out = append(out, st)
	}
	return out, nil
}

// deleteLBPublishedListenerStatusPrefix best-effort removes every observed
// published-listener status record under a load balancer. The etcd client
// exposes no prefix delete, so it ranges the prefix and deletes each key; it
// returns the first delete error so the caller can log it. Ranging then deleting
// is safe here because the records are observed state with no cross-key invariant.
func (s *Store) deleteLBPublishedListenerStatusPrefix(ctx context.Context, lbID uuid.UUID) error {
	items, err := s.c.Range(ctx, lbPublishedListenerStatusPrefix(lbID))
	if err != nil {
		return err
	}
	var firstErr error
	for _, kv := range items {
		if _, derr := s.c.Delete(ctx, kv.Key); derr != nil && firstErr == nil {
			firstErr = derr
		}
	}
	return firstErr
}
