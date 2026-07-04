// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

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

// lbPublishedListenerStatusRootPrefix is the prefix over every LB's listener
// status records, used by the node-scoped reap (there is no per-node index).
func lbPublishedListenerStatusRootPrefix() string {
	return etcd.Key("lb_published_listener_status") + "/"
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

// collectLBPublishedListenerStatusOpsForNode returns delete ops for every
// published-listener status record keyed on the given node, so a deleted node
// leaks no rows (there is no per-node index and no other reaper). The key is
// lb_published_listener_status/<lbID>/<nodeID>, so it ranges the whole root
// prefix and matches keys whose last path segment equals nodeID; the match is
// keys-only (no value decode) so an undecodable value for the deleted node is
// still reaped and the exact UUID suffix cannot over-delete another node's rows.
// A full-prefix scan is acceptable because node delete is rare.
func (s *Store) collectLBPublishedListenerStatusOpsForNode(ctx context.Context, nodeID uuid.UUID) ([]clientv3.Op, error) {
	items, err := s.c.Range(ctx, lbPublishedListenerStatusRootPrefix())
	if err != nil {
		return nil, err
	}
	want := nodeID.String()
	var ops []clientv3.Op
	for _, kv := range items {
		if idx := strings.LastIndex(kv.Key, "/"); idx >= 0 && kv.Key[idx+1:] == want {
			ops = append(ops, clientv3.OpDelete(kv.Key))
		}
	}
	return ops, nil
}
