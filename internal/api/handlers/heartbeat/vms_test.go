// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// vmRuntimeSpy records the vm_runtime writes applyVMs makes and stubs the two
// id filters it gates on. It embeds store.HeartbeatProjection (left nil) so it
// satisfies the interface while implementing only the methods applyVMs
// exercises; any other call would panic, the desired failure mode for an
// unexpected projection step.
type vmRuntimeSpy struct {
	store.HeartbeatProjection
	// existing is the set FilterExistingVMIDs returns (ids with a live vms row).
	existing []uuid.UUID
	// pinned is the set FilterVMIDsPinnedToNode returns (ids pinned to the
	// reporting node).
	pinned []uuid.UUID
	// gotPinnedNodeID and gotPinnedIDs record the arguments of the last
	// FilterVMIDsPinnedToNode call, so the test can assert applyVMs wires the
	// reporting node id (the gate's authority) and the reported vm ids into
	// the placement gate rather than some other values.
	gotPinnedNodeID uuid.UUID
	gotPinnedIDs    []uuid.UUID
	// upserts records UpsertVMRuntime calls in order.
	upserts []store.UpsertVMRuntimeParams
}

func (s *vmRuntimeSpy) FilterExistingVMIDs(_ context.Context, _ []uuid.UUID) ([]uuid.UUID, error) {
	return s.existing, nil
}

func (s *vmRuntimeSpy) FilterVMIDsPinnedToNode(_ context.Context, nodeID uuid.UUID, ids []uuid.UUID) ([]uuid.UUID, error) {
	s.gotPinnedNodeID = nodeID
	s.gotPinnedIDs = ids
	return s.pinned, nil
}

func (s *vmRuntimeSpy) UpsertVMRuntime(_ context.Context, arg store.UpsertVMRuntimeParams) error {
	s.upserts = append(s.upserts, arg)
	return nil
}

// TestApplyVMsPlacementGate verifies the placement-authority seam of the
// heartbeat VM projection (audit H2): a node's runtime claim is written through
// UpsertVMRuntime only when the VM both exists and is pinned to the reporting
// node. A VM pinned to another node, an unscheduled VM (nil pin), and an
// unknown VM are all skipped fail-closed, so a heartbeat can never move
// vm_runtime.current_node_id to a node the scheduler did not place the VM on.
func TestApplyVMsPlacementGate(t *testing.T) {
	reportingNode := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	pinnedHere := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	pinnedElsewhere := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	unpinned := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	unknown := uuid.MustParse("44444444-4444-4444-4444-444444444444")

	pid := int32(4242)
	gen := int64(7)

	tests := []struct {
		name     string
		existing []uuid.UUID
		pinned   []uuid.UUID
		reports  []vmReport
		want     []store.UpsertVMRuntimeParams
	}{
		{
			name:     "vm pinned to reporting node is upserted",
			existing: []uuid.UUID{pinnedHere},
			pinned:   []uuid.UUID{pinnedHere},
			reports: []vmReport{{
				VMUUID:             pinnedHere,
				Phase:              "running",
				ObservedGeneration: &gen,
				QEMUPID:            &pid,
			}},
			want: []store.UpsertVMRuntimeParams{{
				VmID:               pinnedHere,
				CurrentNodeID:      &reportingNode,
				Phase:              store.VMPhase("running"),
				ObservedGeneration: gen,
				QEMUPID:            &pid,
			}},
		},
		{
			name:     "vm pinned to another node is skipped",
			existing: []uuid.UUID{pinnedElsewhere},
			pinned:   []uuid.UUID{},
			reports:  []vmReport{{VMUUID: pinnedElsewhere, Phase: "running"}},
			want:     nil,
		},
		{
			name:     "unscheduled vm with nil pin is skipped",
			existing: []uuid.UUID{unpinned},
			pinned:   []uuid.UUID{},
			reports:  []vmReport{{VMUUID: unpinned, Phase: "running"}},
			want:     nil,
		},
		{
			name:     "unknown vm is skipped",
			existing: []uuid.UUID{},
			pinned:   []uuid.UUID{},
			reports:  []vmReport{{VMUUID: unknown, Phase: "running"}},
			want:     nil,
		},
		{
			name:     "mixed batch upserts only the vm pinned here",
			existing: []uuid.UUID{pinnedHere, pinnedElsewhere},
			pinned:   []uuid.UUID{pinnedHere},
			reports: []vmReport{
				{VMUUID: pinnedElsewhere, Phase: "running"},
				{VMUUID: pinnedHere, Phase: "running"},
			},
			want: []store.UpsertVMRuntimeParams{{
				VmID:          pinnedHere,
				CurrentNodeID: &reportingNode,
				Phase:         store.VMPhase("running"),
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spy := &vmRuntimeSpy{existing: tt.existing, pinned: tt.pinned}
			h := newQuietHandler()
			if err := h.applyVMs(context.Background(), spy, reportingNode, tt.reports); err != nil {
				t.Fatalf("applyVMs(...) = %v, want nil", err)
			}
			if diff := cmp.Diff(tt.want, spy.upserts); diff != "" {
				t.Errorf("applyVMs(...) UpsertVMRuntime calls mismatch (-want +got):\n%s", diff)
			}
			// Call-site wiring: the gate's authority is the node id applyVMs
			// was invoked with, and the candidate set is the reported vm ids
			// (in report order). A regression passing a wrong node id or a
			// wrong id set into FilterVMIDsPinnedToNode fails here.
			if spy.gotPinnedNodeID != reportingNode {
				t.Errorf("applyVMs passed nodeID = %v to FilterVMIDsPinnedToNode, want %v", spy.gotPinnedNodeID, reportingNode)
			}
			wantIDs := make([]uuid.UUID, 0, len(tt.reports))
			for _, r := range tt.reports {
				wantIDs = append(wantIDs, r.VMUUID)
			}
			if diff := cmp.Diff(wantIDs, spy.gotPinnedIDs); diff != "" {
				t.Errorf("applyVMs FilterVMIDsPinnedToNode ids mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
