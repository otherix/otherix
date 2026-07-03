// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// lbListenerUpsert records one UpsertLBPublishedListenerStatus call so the
// ingest tests can assert exactly which (lb, port) bind verdicts
// applyPublishedListeners wrote.
type lbListenerUpsert struct {
	lbID       uuid.UUID
	nodeID     uuid.UUID
	port       int32
	bound      bool
	errMsg     string
	reportedAt time.Time
}

// publishedListenerSpy stubs the one read applyPublishedListeners gates on
// (LoadBalancerByID) and records the writes it makes
// (UpsertLBPublishedListenerStatus). It embeds store.HeartbeatProjection (left
// nil) so it satisfies the interface while implementing only the methods
// applyPublishedListeners exercises; any other call would panic, the desired
// failure mode for an unexpected projection step.
type publishedListenerSpy struct {
	store.HeartbeatProjection
	// liveLBs is the set of load balancer ids LoadBalancerByID resolves as live;
	// any id absent from it returns store.ErrNotFound (soft-deleted / missing).
	liveLBs map[uuid.UUID]struct{}
	// upserts records UpsertLBPublishedListenerStatus calls in order.
	upserts []lbListenerUpsert
}

func (s *publishedListenerSpy) LoadBalancerByID(_ context.Context, id uuid.UUID) (store.LoadBalancer, error) {
	if _, ok := s.liveLBs[id]; ok {
		return store.LoadBalancer{ID: id}, nil
	}
	return store.LoadBalancer{}, store.ErrNotFound
}

func (s *publishedListenerSpy) UpsertLBPublishedListenerStatus(_ context.Context, lbID, nodeID uuid.UUID, port int32, bound bool, errMsg string, reportedAt time.Time) error {
	s.upserts = append(s.upserts, lbListenerUpsert{lbID: lbID, nodeID: nodeID, port: port, bound: bound, errMsg: errMsg, reportedAt: reportedAt})
	return nil
}

// TestApplyPublishedListenersWritesStatus verifies the observed listener
// up-channel: each report is written through UpsertLBPublishedListenerStatus
// keyed by the reporting node id (no per-VM placement gate — published listeners
// are node-scoped, not VM-scoped). Both a bound and a failed listener are
// persisted, stamped with the CP receive time (never the agent's clock).
func TestApplyPublishedListenersWritesStatus(t *testing.T) {
	reportingNode := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	lbBound := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	lbFailed := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")

	spy := &publishedListenerSpy{
		liveLBs: map[uuid.UUID]struct{}{lbBound: {}, lbFailed: {}},
	}
	h := newQuietHandler()

	reports := []publishedListenerReport{
		{LBID: lbBound, Port: 8080, Bound: true},
		{LBID: lbFailed, Port: 8081, Bound: false, Error: "bind: address already in use"},
	}

	before := time.Now()
	if err := h.applyPublishedListeners(context.Background(), spy, reportingNode, reports); err != nil {
		t.Fatalf("applyPublishedListeners(...) = %v, want nil", err)
	}
	after := time.Now()

	if len(spy.upserts) != 2 {
		t.Fatalf("applyPublishedListeners wrote %d rows, want 2", len(spy.upserts))
	}
	for i, got := range spy.upserts {
		if got.nodeID != reportingNode {
			t.Errorf("upsert[%d] nodeID = %v, want %v (reporting node)", i, got.nodeID, reportingNode)
		}
		if got.reportedAt.Before(before) || got.reportedAt.After(after) {
			t.Errorf("upsert[%d] reportedAt = %v, want within [%v, %v]", i, got.reportedAt, before, after)
		}
	}
	if b := spy.upserts[0]; b.lbID != lbBound || b.port != 8080 || !b.bound || b.errMsg != "" {
		t.Errorf("upsert[0] = %+v, want lb=%v port=8080 bound err=''", b, lbBound)
	}
	if f := spy.upserts[1]; f.lbID != lbFailed || f.port != 8081 || f.bound || f.errMsg == "" {
		t.Errorf("upsert[1] = %+v, want lb=%v port=8081 unbound with error", f, lbFailed)
	}
}

// TestApplyPublishedListenersSkipsDeletedLB asserts the write-vs-delete-race
// gate: a heartbeat naming a soft-deleted load balancer
// (LoadBalancerByID -> ErrNotFound) writes nothing, so an in-flight heartbeat
// cannot re-create the status row DeleteLoadBalancer's cascade removed.
func TestApplyPublishedListenersSkipsDeletedLB(t *testing.T) {
	reportingNode := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	deletedLB := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")

	spy := &publishedListenerSpy{
		liveLBs: map[uuid.UUID]struct{}{}, // deletedLB resolves to ErrNotFound.
	}
	h := newQuietHandler()

	reports := []publishedListenerReport{{LBID: deletedLB, Port: 8080, Bound: true}}
	if err := h.applyPublishedListeners(context.Background(), spy, reportingNode, reports); err != nil {
		t.Fatalf("applyPublishedListeners(...) = %v, want nil", err)
	}
	if len(spy.upserts) != 0 {
		t.Fatalf("applyPublishedListeners wrote %d rows for a deleted LB, want 0", len(spy.upserts))
	}
}
