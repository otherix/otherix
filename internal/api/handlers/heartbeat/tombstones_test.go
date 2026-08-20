// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// tombstoneSpy answers the two projection reads the teardown decision depends
// on from the calling test's own fixture.
//
// It deliberately does not reuse vmRuntimeSpy: that fake answers VMSoftDeleted
// "not deleted" for every id, which would rob both directions of these
// assertions of their meaning - a tombstone case would fail for the wrong
// reason, and a no-tombstone case would pass even with the whole feature
// absent.
type tombstoneSpy struct {
	store.HeartbeatProjection
	// existing is what FilterExistingVMIDs returns: the ids the report loop
	// recognises as live and therefore must never look up.
	existing []uuid.UUID
	// deleted maps an id to the name VMSoftDeleted reports for a present,
	// deletion-stamped row. An id in neither map reads as (false, "", nil),
	// which models a reported id with no row at all.
	deleted map[uuid.UUID]string
	// readErr maps an id to the error VMSoftDeleted returns for it.
	readErr map[uuid.UUID]error
	// lookups records every id VMSoftDeleted was asked about, in order.
	lookups []uuid.UUID
}

func (s *tombstoneSpy) FilterExistingVMIDs(context.Context, []uuid.UUID) ([]uuid.UUID, error) {
	return s.existing, nil
}

// FilterVMIDsPinnedToNode reports nothing pinned to the reporting node. None of
// these tests drive the runtime claim, and a teardown signal must not depend on
// the pin: if the tombstone path ever fell behind the placement gate, every
// assertion below that expects a tombstone would go empty.
func (s *tombstoneSpy) FilterVMIDsPinnedToNode(context.Context, uuid.UUID, []uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

func (s *tombstoneSpy) VMSoftDeleted(_ context.Context, id uuid.UUID) (bool, string, error) {
	s.lookups = append(s.lookups, id)
	if err, ok := s.readErr[id]; ok {
		return false, "", err
	}
	name, ok := s.deleted[id]
	return ok, name, nil
}

func newCapturingHandler(buf *bytes.Buffer) *Handler {
	return &Handler{log: slog.New(slog.NewTextHandler(buf, nil))}
}

// runVMReports drives the real observed-report entry point (the sequence
// project calls) rather than applyVMs directly, so the assertions cover the
// whole path from the report loop to the heartbeatOutcome the response body is
// built from. The two *Unavailable flags short-circuit the blob-inventory
// steps, which are irrelevant here and would otherwise reach the nil embedded
// projection.
func runVMReports(t *testing.T, h *Handler, hp store.HeartbeatProjection, nodeID uuid.UUID, reports []vmReport) heartbeatOutcome {
	t.Helper()
	var outcome heartbeatOutcome
	body := &requestBody{
		VMs:                   reports,
		BlobsUnavailable:      true,
		ImageBlobsUnavailable: true,
	}
	if err := h.applyObservedReports(context.Background(), hp, nodeID, body, &outcome); err != nil {
		t.Fatalf("applyObservedReports(...) = %v, want nil", err)
	}
	return outcome
}

func vmReportsFor(ids ...uuid.UUID) []vmReport {
	out := make([]vmReport, 0, len(ids))
	for _, id := range ids {
		out = append(out, vmReport{VMUUID: id, Phase: "running"})
	}
	return out
}

// TestHeartbeat_ReportedSoftDeletedVMYieldsTombstone asserts a node reporting a
// VM whose row is soft-deleted receives a tombstone naming that UUID, and pins
// the wire names the agent decodes.
func TestHeartbeat_ReportedSoftDeletedVMYieldsTombstone(t *testing.T) {
	node := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	vmID := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	spy := &tombstoneSpy{deleted: map[uuid.UUID]string{vmID: "web-1"}}
	outcome := runVMReports(t, newQuietHandler(), spy, node, vmReportsFor(vmID))

	want := []vmTombstone{{VMID: vmID, VMName: "web-1"}}
	if diff := cmp.Diff(want, outcome.vmTombstones); diff != "" {
		t.Errorf("tombstones mismatch (-want +got):\n%s", diff)
	}

	// The response field names are a cross-component contract: the agent
	// decodes vm_tombstones[].vm_id / .vm_name.
	blob, err := json.Marshal(responseBody{VMTombstones: outcome.vmTombstones})
	if err != nil {
		t.Fatalf("json.Marshal(responseBody) = %v, want nil", err)
	}
	wantJSON := fmt.Sprintf(`"vm_tombstones":[{"vm_id":%q,"vm_name":"web-1"}]`, vmID)
	if !strings.Contains(string(blob), wantJSON) {
		t.Errorf("response body = %s, want it to contain %s", blob, wantJSON)
	}
}

// TestHeartbeat_ReportedLiveVMYieldsNoTombstone asserts the ordinary case emits
// nothing, so a running VM is never told to tear itself down. The fixture
// answers "deleted" for the live VM too: the handler must never ask about a
// recognised row, and a regression that looked one up would order a running
// guest destroyed.
func TestHeartbeat_ReportedLiveVMYieldsNoTombstone(t *testing.T) {
	node := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	live := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	gone := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	spy := &tombstoneSpy{
		existing: []uuid.UUID{live},
		deleted:  map[uuid.UUID]string{live: "must-not-be-asked", gone: "gone-1"},
	}
	outcome := runVMReports(t, newQuietHandler(), spy, node, vmReportsFor(live, gone))

	// The deleted VM in the same batch keeps this assertion honest: it fails if
	// the feature is absent as well as if the live VM is torn down.
	want := []vmTombstone{{VMID: gone, VMName: "gone-1"}}
	if diff := cmp.Diff(want, outcome.vmTombstones); diff != "" {
		t.Errorf("tombstones mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]uuid.UUID{gone}, spy.lookups); diff != "" {
		t.Errorf("VMSoftDeleted lookups mismatch (-want +got):\n%s", diff)
	}
}

// TestHeartbeat_ReportedUnknownVMWithNoRowYieldsNoTombstone asserts absence is
// not a trigger: a reported UUID with no row at all yields nothing, while a
// soft-deleted one in the same batch still does.
func TestHeartbeat_ReportedUnknownVMWithNoRowYieldsNoTombstone(t *testing.T) {
	node := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	noRow := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	deleted := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	spy := &tombstoneSpy{deleted: map[uuid.UUID]string{deleted: "gone-1"}}
	outcome := runVMReports(t, newQuietHandler(), spy, node, vmReportsFor(noRow, deleted))

	want := []vmTombstone{{VMID: deleted, VMName: "gone-1"}}
	if diff := cmp.Diff(want, outcome.vmTombstones); diff != "" {
		t.Errorf("tombstones mismatch (-want +got):\n%s", diff)
	}
	// The row-less id was read and answered "not deleted" - the decision came
	// from a positive read, not from the id being unrecognised.
	if diff := cmp.Diff([]uuid.UUID{noRow, deleted}, spy.lookups); diff != "" {
		t.Errorf("VMSoftDeleted lookups mismatch (-want +got):\n%s", diff)
	}
}

// TestHeartbeat_TombstoneLookupErrorYieldsNoTombstone asserts a failed read
// emits nothing and does not stop the rest of the batch: uncertainty resolves
// toward leaving the guest running.
func TestHeartbeat_TombstoneLookupErrorYieldsNoTombstone(t *testing.T) {
	node := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	unreadable := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	deleted := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	spy := &tombstoneSpy{
		// The unreadable row would read as deleted if the error were ignored.
		deleted: map[uuid.UUID]string{unreadable: "unreadable-1", deleted: "gone-1"},
		readErr: map[uuid.UUID]error{unreadable: errors.New("boom")},
	}
	buf := &bytes.Buffer{}
	outcome := runVMReports(t, newCapturingHandler(buf), spy, node, vmReportsFor(unreadable, deleted))

	want := []vmTombstone{{VMID: deleted, VMName: "gone-1"}}
	if diff := cmp.Diff(want, outcome.vmTombstones); diff != "" {
		t.Errorf("tombstones mismatch (-want +got):\n%s", diff)
	}
	if !strings.Contains(buf.String(), "tombstone lookup failed") {
		t.Errorf("logs = %q, want a line reporting the failed lookup", buf.String())
	}
}

// TestHeartbeat_TombstoneIsIndependentOfThePinnedNodeGate asserts a node
// reporting a soft-deleted VM it was never pinned to still receives the
// tombstone. The pin gate exists to stop a node claiming runtime it does not
// own; a teardown is not a runtime claim, and the pin is routinely nil in the
// cases that matter (an unpinned VM, or one orphaned by a force-deleted node).
func TestHeartbeat_TombstoneIsIndependentOfThePinnedNodeGate(t *testing.T) {
	node := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	vmID := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	// FilterVMIDsPinnedToNode returns nothing, so the placement gate admits no
	// runtime claim for this report. UpsertVMRuntime is not stubbed on the spy
	// either, so a claim attempt would panic on the nil embedded projection -
	// the tombstone must come out without one.
	spy := &tombstoneSpy{deleted: map[uuid.UUID]string{vmID: "orphan-1"}}
	outcome := runVMReports(t, newQuietHandler(), spy, node, vmReportsFor(vmID))

	want := []vmTombstone{{VMID: vmID, VMName: "orphan-1"}}
	if diff := cmp.Diff(want, outcome.vmTombstones); diff != "" {
		t.Errorf("tombstones mismatch (-want +got):\n%s", diff)
	}
}

// TestHeartbeat_EveryUnrecognisedIDIsResolved asserts a large batch is resolved
// in full rather than by prefix. The agent reports in UUID order, so a
// per-tick ceiling would let a run of ids the CP has no row for permanently
// occupy the resolved prefix and starve the deleted VM sorting above them -
// its guest would then leak forever, which is the failure this signal exists to
// close. The deleted VM is placed LAST so a prefix-shaped regression fails.
func TestHeartbeat_EveryUnrecognisedIDIsResolved(t *testing.T) {
	node := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")

	const rowless = 200
	ids := make([]uuid.UUID, 0, rowless+1)
	for range rowless {
		ids = append(ids, uuid.New())
	}
	last := uuid.New()
	ids = append(ids, last)

	spy := &tombstoneSpy{deleted: map[uuid.UUID]string{last: "gone-1"}}
	outcome := runVMReports(t, newQuietHandler(), spy, node, vmReportsFor(ids...))

	want := []vmTombstone{{VMID: last, VMName: "gone-1"}}
	if diff := cmp.Diff(want, outcome.vmTombstones); diff != "" {
		t.Errorf("tombstones mismatch (-want +got):\n%s", diff)
	}
	if len(spy.lookups) != len(ids) {
		t.Errorf("VMSoftDeleted lookups = %d, want %d (one per reported id)", len(spy.lookups), len(ids))
	}
}
