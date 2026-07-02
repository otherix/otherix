// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// lb_backend_health/<lbID>/<vmID> -> JSON store.LBBackendHealth. Observed state,
// heartbeat-fed (last-writer-wins per pair, no guard), read by the connect
// eligibility path and the lb get view. The per-lb prefix range gives both reads
// one round trip; DeleteLoadBalancer cascades the whole <lbID>/ prefix.
func lbBackendHealthKey(lbID, vmID uuid.UUID) string {
	return etcd.Key("lb_backend_health", lbID.String(), vmID.String())
}

func lbBackendHealthPrefix(lbID uuid.UUID) string {
	return etcd.Key("lb_backend_health", lbID.String()) + "/"
}

// UpsertLBBackendHealth writes the observed verdict for (lbID, vmID). A blind
// put, last-writer-wins per heartbeat.
func (s *Store) UpsertLBBackendHealth(ctx context.Context, lbID, vmID uuid.UUID, healthy bool, reportedAt time.Time) error {
	return s.c.PutJSON(ctx, lbBackendHealthKey(lbID, vmID), store.LBBackendHealth{
		Healthy: healthy, ReportedAt: reportedAt.UTC(),
	})
}

// ListLBBackendHealth returns every observed backend-health record for a load
// balancer, keyed by backend VM id. An empty map means nothing reported yet.
func (s *Store) ListLBBackendHealth(ctx context.Context, lbID uuid.UUID) (map[uuid.UUID]store.LBBackendHealth, error) {
	items, err := s.c.Range(ctx, lbBackendHealthPrefix(lbID))
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]store.LBBackendHealth, len(items))
	for _, kv := range items {
		vmID, perr := uuid.Parse(kv.Key[strings.LastIndexByte(kv.Key, '/')+1:])
		if perr != nil {
			continue
		}
		var h store.LBBackendHealth
		if !s.decodeOrQuarantine(ctx, kv.Key, kv.Value, &h, "lb_backend_health") {
			continue
		}
		out[vmID] = h
	}
	return out, nil
}

// deleteLBBackendHealthPrefix best-effort removes every observed backend-health
// record under a load balancer. The etcd client exposes no prefix delete, so it
// ranges the prefix and deletes each key; it returns the first delete error so
// the caller can log it. Ranging then deleting is safe here because the records
// are observed state with no cross-key invariant.
func (s *Store) deleteLBBackendHealthPrefix(ctx context.Context, lbID uuid.UUID) error {
	items, err := s.c.Range(ctx, lbBackendHealthPrefix(lbID))
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
