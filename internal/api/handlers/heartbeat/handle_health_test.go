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

// lbHealthUpsert records one UpsertLBBackendHealth call so the gate tests can
// assert exactly which (lb, backend-VM) verdicts applyHealthChecks wrote.
type lbHealthUpsert struct {
	lbID       uuid.UUID
	vmID       uuid.UUID
	healthy    bool
	reportedAt time.Time
}

// healthCheckSpy stubs the two reads applyHealthChecks gates on
// (FilterVMIDsPinnedToNode, LoadBalancerByID) and records the writes it makes
// (UpsertLBBackendHealth). It embeds store.HeartbeatProjection (left nil) so it
// satisfies the interface while implementing only the methods applyHealthChecks
// exercises; any other call would panic, the desired failure mode for an
// unexpected projection step.
type healthCheckSpy struct {
	store.HeartbeatProjection
	// pinned is the set FilterVMIDsPinnedToNode returns (VM ids pinned to the
	// reporting node, the placement-authority gate).
	pinned []uuid.UUID
	// liveLBs is the set of load balancer ids LoadBalancerByID resolves as live;
	// any id absent from it returns store.ErrNotFound (soft-deleted / missing),
	// exercising the write-vs-delete-race gate.
	liveLBs map[uuid.UUID]struct{}
	// gotPinnedNodeID and gotPinnedIDs record the last FilterVMIDsPinnedToNode
	// arguments so the test can assert applyHealthChecks wires the reporting node
	// id and the reported backend VM ids into the gate.
	gotPinnedNodeID uuid.UUID
	gotPinnedIDs    []uuid.UUID
	// upserts records UpsertLBBackendHealth calls in order.
	upserts []lbHealthUpsert
}

func (s *healthCheckSpy) FilterVMIDsPinnedToNode(_ context.Context, nodeID uuid.UUID, ids []uuid.UUID) ([]uuid.UUID, error) {
	s.gotPinnedNodeID = nodeID
	s.gotPinnedIDs = ids
	return s.pinned, nil
}

func (s *healthCheckSpy) LoadBalancerByID(_ context.Context, id uuid.UUID) (store.LoadBalancer, error) {
	if _, ok := s.liveLBs[id]; ok {
		return store.LoadBalancer{ID: id}, nil
	}
	return store.LoadBalancer{}, store.ErrNotFound
}

func (s *healthCheckSpy) UpsertLBBackendHealth(_ context.Context, lbID, vmID uuid.UUID, healthy bool, reportedAt time.Time) error {
	s.upserts = append(s.upserts, lbHealthUpsert{lbID: lbID, vmID: vmID, healthy: healthy, reportedAt: reportedAt})
	return nil
}

// TestApplyHealthChecksPlacementGate verifies the placement-authority seam of
// the backend-health up-channel: a verdict is written through
// UpsertLBBackendHealth only for a backend VM pinned to the reporting node. A
// verdict naming a VM pinned to a DIFFERENT node is dropped fail-closed, so a
// node cannot forge health for another node's VM. The surviving write is stamped
// with the CP receive time (never the agent's clock).
func TestApplyHealthChecksPlacementGate(t *testing.T) {
	reportingNode := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	lb := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	pinnedHere := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	pinnedElsewhere := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	spy := &healthCheckSpy{
		pinned:  []uuid.UUID{pinnedHere},
		liveLBs: map[uuid.UUID]struct{}{lb: {}},
	}
	h := newQuietHandler()

	reports := []healthCheckReport{
		{LBID: lb, VMID: pinnedElsewhere, Healthy: true},
		{LBID: lb, VMID: pinnedHere, Healthy: true},
	}

	before := time.Now()
	if err := h.applyHealthChecks(context.Background(), spy, reportingNode, reports); err != nil {
		t.Fatalf("applyHealthChecks(...) = %v, want nil", err)
	}
	after := time.Now()

	if len(spy.upserts) != 1 {
		t.Fatalf("applyHealthChecks wrote %d verdicts, want 1 (only the pinned VM)", len(spy.upserts))
	}
	got := spy.upserts[0]
	if got.lbID != lb || got.vmID != pinnedHere || !got.healthy {
		t.Errorf("applyHealthChecks wrote (lb=%v vm=%v healthy=%v), want (lb=%v vm=%v healthy=true)",
			got.lbID, got.vmID, got.healthy, lb, pinnedHere)
	}
	// CP-authoritative freshness: reported_at is stamped from the CP clock at
	// receipt, so it lands inside the call window, never a value the agent chose.
	if got.reportedAt.Before(before) || got.reportedAt.After(after) {
		t.Errorf("applyHealthChecks reported_at = %v, want within [%v, %v]", got.reportedAt, before, after)
	}

	// Call-site wiring: the gate's authority is the reporting node id, and the
	// candidate set is the reported backend VM ids in report order.
	if spy.gotPinnedNodeID != reportingNode {
		t.Errorf("applyHealthChecks passed nodeID = %v to FilterVMIDsPinnedToNode, want %v", spy.gotPinnedNodeID, reportingNode)
	}
	wantIDs := []uuid.UUID{pinnedElsewhere, pinnedHere}
	if len(spy.gotPinnedIDs) != len(wantIDs) {
		t.Fatalf("applyHealthChecks passed %d ids to FilterVMIDsPinnedToNode, want %d", len(spy.gotPinnedIDs), len(wantIDs))
	}
	for i, id := range wantIDs {
		if spy.gotPinnedIDs[i] != id {
			t.Errorf("applyHealthChecks FilterVMIDsPinnedToNode id[%d] = %v, want %v", i, spy.gotPinnedIDs[i], id)
		}
	}
}

// TestApplyHealthChecksSkipsDeletedLB asserts the write-vs-delete-race gate: a
// heartbeat naming a soft-deleted load balancer (LoadBalancerByID -> ErrNotFound)
// for an otherwise-pinned backend VM writes nothing. Without the gate an
// in-flight heartbeat would re-create the health row DeleteLoadBalancer's cascade
// removed, and that orphan would then leak forever (the LB is never re-listed).
func TestApplyHealthChecksSkipsDeletedLB(t *testing.T) {
	reportingNode := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	deletedLB := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	pinnedHere := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	spy := &healthCheckSpy{
		pinned:  []uuid.UUID{pinnedHere},
		liveLBs: map[uuid.UUID]struct{}{}, // deletedLB resolves to ErrNotFound.
	}
	h := newQuietHandler()

	reports := []healthCheckReport{{LBID: deletedLB, VMID: pinnedHere, Healthy: true}}
	if err := h.applyHealthChecks(context.Background(), spy, reportingNode, reports); err != nil {
		t.Fatalf("applyHealthChecks(...) = %v, want nil", err)
	}
	if len(spy.upserts) != 0 {
		t.Fatalf("applyHealthChecks wrote %d verdicts for a deleted LB, want 0 (not resurrected)", len(spy.upserts))
	}
}
