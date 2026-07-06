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
	// activeMigrations maps a vm id to the active (non-terminal) migration the
	// epoch fence consults. A vm absent from the map has no active migration, so
	// ActiveMigrationForVM reports (zero, false, nil) and applyVMs claims the
	// reporting node as usual.
	activeMigrations map[uuid.UUID]store.Migration
	// vmRows is what VMWithRev returns per id (all at rev vmRev). An id absent
	// from the map returns store.ErrNotFound, modelling a soft-deleted or missing
	// vms row - applyVMReport reads the pin and the CAS rev from this call.
	vmRows map[uuid.UUID]store.VM
	vmRev  int64
}

func (s *vmRuntimeSpy) VMWithRev(_ context.Context, id uuid.UUID) (store.VM, int64, error) {
	vm, ok := s.vmRows[id]
	if !ok {
		return store.VM{}, 0, store.ErrNotFound
	}
	return vm, s.vmRev, nil
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

func (s *vmRuntimeSpy) ActiveMigrationForVM(_ context.Context, vmID uuid.UUID) (store.Migration, bool, error) {
	mig, ok := s.activeMigrations[vmID]
	return mig, ok, nil
}

// TestApplyVMsPlacementGate verifies the placement-authority seam of the
// heartbeat VM projection: a node's runtime claim is written through
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
	// The rev VMWithRev reports for the pinned rows; applyVMReport threads it into
	// the runtime write so UpsertVMRuntime can compare on it.
	wantRev := int64(55)

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
				VMRowModRevision:   55,
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
				VmID:             pinnedHere,
				CurrentNodeID:    &reportingNode,
				Phase:            store.VMPhase("running"),
				VMRowModRevision: 55,
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Every pinned id re-reads (via VMWithRev) as a row pinned to the
			// reporting node at wantRev, so the per-VM re-check admits it.
			vmRows := make(map[uuid.UUID]store.VM, len(tt.pinned))
			for _, id := range tt.pinned {
				pin := reportingNode
				vmRows[id] = store.VM{ID: id, PinnedNodeID: &pin}
			}
			spy := &vmRuntimeSpy{existing: tt.existing, pinned: tt.pinned, vmRows: vmRows, vmRev: wantRev}
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

// TestApplyVMsFreezesCurrentNodeIDDuringMigration asserts the epoch fence (ADR
// 0035 req 1): while a VM has an active migration, a heartbeat from the TARGET
// node must NOT move current_node_id off the source. The placement gate admits
// the migration target (FilterVMIDsPinnedToNode returns it), so the target's
// report reaches applyVMs; without the fence its UpsertVMRuntime would set
// current_node_id to the target and flap the overlay FDB. Phase and the rest of
// the runtime row must still update from the target's report - only the node is
// frozen to the migration source.
func TestApplyVMsFreezesCurrentNodeIDDuringMigration(t *testing.T) {
	source := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	target := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	vmID := uuid.MustParse("33333333-3333-3333-3333-333333333333")

	pid := int32(9001)
	gen := int64(3)

	spy := &vmRuntimeSpy{
		existing: []uuid.UUID{vmID},
		// The placement gate admits the migration target during a move.
		pinned: []uuid.UUID{vmID},
		// The pin is still the source until cutover; the target is admitted via
		// the active migration, whose target must be this reporting node.
		vmRows: map[uuid.UUID]store.VM{vmID: {ID: vmID, PinnedNodeID: &source}},
		vmRev:  71,
		activeMigrations: map[uuid.UUID]store.Migration{
			vmID: {SourceNodeID: &source, TargetNodeID: &target},
		},
	}
	h := newQuietHandler()

	// The TARGET node reports the incoming VM as "migrating".
	reports := []vmReport{{
		VMUUID:             vmID,
		Phase:              string(store.VMPhase("migrating")),
		ObservedGeneration: &gen,
		QEMUPID:            &pid,
	}}
	if err := h.applyVMs(context.Background(), spy, target, reports); err != nil {
		t.Fatalf("applyVMs(...) = %v, want nil", err)
	}

	want := []store.UpsertVMRuntimeParams{{
		VmID: vmID,
		// Frozen to the migration source, NOT the reporting (target) node.
		CurrentNodeID:      &source,
		Phase:              store.VMPhase("migrating"),
		ObservedGeneration: gen,
		QEMUPID:            &pid,
		VMRowModRevision:   71,
	}}
	if diff := cmp.Diff(want, spy.upserts); diff != "" {
		t.Errorf("applyVMs(...) UpsertVMRuntime calls mismatch (-want +got):\n%s", diff)
	}
}

// TestApplyVMReportSkipsWhenPinMovedOrDeleted covers the fresh-read seam that
// closes the two hot-path TOCTOUs: the batch placement gate admits a report, but
// by the time applyVMReport runs its per-VM VMWithRev read the vms row has moved
// under it. In both sub-cases applyVMReport must SKIP (write nothing) rather than
// upsert a stale claim - a stale claim would either resurrect a runtime row on a
// deleted VM or regress current_node_id back to the source after a cutover.
func TestApplyVMReportSkipsWhenPinMovedOrDeleted(t *testing.T) {
	reportingNode := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	otherNode := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	vmID := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	t.Run("pin moved to another node (cutover raced ahead of the fresh read)", func(t *testing.T) {
		spy := &vmRuntimeSpy{
			existing: []uuid.UUID{vmID},
			// Stale batch gate still admits the reporting node...
			pinned: []uuid.UUID{vmID},
			vmRev:  42,
			// ...but the fresh row shows the pin already moved to otherNode and no
			// active migration (the guard was deleted at cutover).
			vmRows: map[uuid.UUID]store.VM{vmID: {ID: vmID, PinnedNodeID: &otherNode}},
		}
		h := newQuietHandler()
		if err := h.applyVMs(context.Background(), spy, reportingNode, []vmReport{{VMUUID: vmID, Phase: "running"}}); err != nil {
			t.Fatalf("applyVMs(...) = %v, want nil", err)
		}
		if len(spy.upserts) != 0 {
			t.Errorf("applyVMs upserted %d rows for a pin-moved VM; want 0 (skip)", len(spy.upserts))
		}
	})

	t.Run("vms row deleted (VMWithRev returns ErrNotFound)", func(t *testing.T) {
		spy := &vmRuntimeSpy{
			existing: []uuid.UUID{vmID},
			pinned:   []uuid.UUID{vmID},
			vmRev:    42,
			// vmRows has no entry for vmID -> VMWithRev returns store.ErrNotFound.
			vmRows: map[uuid.UUID]store.VM{},
		}
		h := newQuietHandler()
		if err := h.applyVMs(context.Background(), spy, reportingNode, []vmReport{{VMUUID: vmID, Phase: "running"}}); err != nil {
			t.Fatalf("applyVMs(...) = %v, want nil", err)
		}
		if len(spy.upserts) != 0 {
			t.Errorf("applyVMs upserted %d rows for a deleted VM; want 0 (skip)", len(spy.upserts))
		}
	})
}

// TestApplyVMReportClaimAuthorityBranches pins down the remaining arms of the
// claim-authority switch: case A (pinned node) must win WITHOUT consulting the
// migration, and the migration-target arm must fail closed when the source is
// nil or the reporter is not this move's target.
func TestApplyVMReportClaimAuthorityBranches(t *testing.T) {
	reporting := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	source := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	target := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	vmID := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	t.Run("pinned node wins without consulting the migration", func(t *testing.T) {
		// The VM is pinned to the reporting node AND an active migration exists
		// whose source is a different node. Case A must claim the reporting node;
		// if the switch wrongly consulted the migration it would claim source.
		spy := &vmRuntimeSpy{
			existing: []uuid.UUID{vmID},
			pinned:   []uuid.UUID{vmID},
			vmRev:    9,
			vmRows:   map[uuid.UUID]store.VM{vmID: {ID: vmID, PinnedNodeID: &reporting}},
			activeMigrations: map[uuid.UUID]store.Migration{
				vmID: {SourceNodeID: &source, TargetNodeID: &target},
			},
		}
		h := newQuietHandler()
		if err := h.applyVMs(context.Background(), spy, reporting, []vmReport{{VMUUID: vmID, Phase: "running"}}); err != nil {
			t.Fatalf("applyVMs(...) = %v, want nil", err)
		}
		want := []store.UpsertVMRuntimeParams{{
			VmID: vmID, CurrentNodeID: &reporting, Phase: store.VMPhase("running"), VMRowModRevision: 9,
		}}
		if diff := cmp.Diff(want, spy.upserts); diff != "" {
			t.Errorf("claim mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("target arm skips when the migration source is nil", func(t *testing.T) {
		spy := &vmRuntimeSpy{
			existing: []uuid.UUID{vmID},
			pinned:   []uuid.UUID{vmID},
			vmRev:    9,
			vmRows:   map[uuid.UUID]store.VM{vmID: {ID: vmID, PinnedNodeID: &source}},
			activeMigrations: map[uuid.UUID]store.Migration{
				vmID: {TargetNodeID: &target}, // SourceNodeID nil
			},
		}
		h := newQuietHandler()
		if err := h.applyVMs(context.Background(), spy, target, []vmReport{{VMUUID: vmID, Phase: "migrating"}}); err != nil {
			t.Fatalf("applyVMs(...) = %v, want nil", err)
		}
		if len(spy.upserts) != 0 {
			t.Errorf("applyVMs upserted %d rows with a nil migration source; want 0 (skip)", len(spy.upserts))
		}
	})

	t.Run("target arm skips when the reporter is not this move's target", func(t *testing.T) {
		bystander := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")
		spy := &vmRuntimeSpy{
			existing: []uuid.UUID{vmID},
			pinned:   []uuid.UUID{vmID}, // stale gate admits the bystander
			vmRev:    9,
			vmRows:   map[uuid.UUID]store.VM{vmID: {ID: vmID, PinnedNodeID: &source}},
			activeMigrations: map[uuid.UUID]store.Migration{
				vmID: {SourceNodeID: &source, TargetNodeID: &target}, // target != bystander
			},
		}
		h := newQuietHandler()
		if err := h.applyVMs(context.Background(), spy, bystander, []vmReport{{VMUUID: vmID, Phase: "running"}}); err != nil {
			t.Fatalf("applyVMs(...) = %v, want nil", err)
		}
		if len(spy.upserts) != 0 {
			t.Errorf("applyVMs upserted %d rows for a non-target reporter; want 0 (skip)", len(spy.upserts))
		}
	})
}
