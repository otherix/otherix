// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// rehomedSpy answers the reads the re-homed teardown decision depends on. Every
// VM it is asked about is recognised (FilterExistingVMIDs echoes the request)
// and none is pinned to the reporting node, so each report lands on the
// re-homed path rather than the deleted-row path. VMSoftDeleted answers "not
// deleted" for everything, so a tombstone in these tests can only have come
// from the re-homed decision.
type rehomedSpy struct {
	store.HeartbeatProjection
	vms     map[uuid.UUID]store.VM
	vmErr   map[uuid.UUID]error
	nodes   map[uuid.UUID]store.Node
	nodeErr map[uuid.UUID]error
	// pinned is what FilterVMIDsPinnedToNode returns.
	pinned []uuid.UUID
	// vmLookups records every id VMWithRev was asked about, in order.
	vmLookups []uuid.UUID
}

func (s *rehomedSpy) FilterExistingVMIDs(_ context.Context, ids []uuid.UUID) ([]uuid.UUID, error) {
	return ids, nil
}

func (s *rehomedSpy) FilterVMIDsPinnedToNode(context.Context, uuid.UUID, []uuid.UUID) ([]uuid.UUID, error) {
	return s.pinned, nil
}

func (s *rehomedSpy) VMSoftDeleted(context.Context, uuid.UUID) (bool, string, error) {
	return false, "", nil
}

func (s *rehomedSpy) VMWithRev(_ context.Context, id uuid.UUID) (store.VM, int64, error) {
	s.vmLookups = append(s.vmLookups, id)
	if err, ok := s.vmErr[id]; ok {
		return store.VM{}, 0, err
	}
	vm, ok := s.vms[id]
	if !ok {
		return store.VM{}, 0, store.ErrNotFound
	}
	return vm, 1, nil
}

// ActiveMigrationForVM answers "no active migration" so an admitted report
// leaves applyVMReport before the runtime write.
func (s *rehomedSpy) ActiveMigrationForVM(context.Context, uuid.UUID) (store.Migration, bool, error) {
	return store.Migration{}, false, nil
}

func (s *rehomedSpy) NodeByID(_ context.Context, id uuid.UUID) (store.Node, error) {
	if err, ok := s.nodeErr[id]; ok {
		return store.Node{}, err
	}
	n, ok := s.nodes[id]
	if !ok {
		return store.Node{}, store.ErrNotFound
	}
	return n, nil
}

// TestHeartbeat_VMPinnedToALiveNodeYieldsTombstoneOnTheStaleHolder covers the
// duplicate a re-bind under the same UUID leaves behind: the VM was re-homed to
// another node, the old holder comes back and replays the guest from its own
// on-disk record, and two copies of one VM are live at once. The old holder is
// told to tear its copy down.
func TestHeartbeat_VMPinnedToALiveNodeYieldsTombstoneOnTheStaleHolder(t *testing.T) {
	staleHolder := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	home := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	vmID := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	spy := &rehomedSpy{
		vms:   map[uuid.UUID]store.VM{vmID: {ID: vmID, Name: "web-1", PinnedNodeID: &home}},
		nodes: map[uuid.UUID]store.Node{home: {ID: home, Status: store.NodeStatusReady}},
	}
	outcome := runVMReports(t, newQuietHandler(), spy, staleHolder, vmReportsFor(vmID))

	want := []vmTombstone{{VMID: vmID, VMName: "web-1"}}
	if diff := cmp.Diff(want, outcome.vmTombstones); diff != "" {
		t.Errorf("tombstones mismatch (-want +got):\n%s", diff)
	}
}

// TestHeartbeat_RehomedTeardownNeedsALiveHome is the load-bearing safety case.
// A node force-deleted while it ran VMs leaves them pinned to the now-deleted
// node row; when that host is rebuilt and readmitted it holds the ONLY copy of
// each guest. Absence of a live home must never order that copy destroyed.
func TestHeartbeat_RehomedTeardownNeedsALiveHome(t *testing.T) {
	reporter := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	deadHome := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	unreachableHome := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	unscheduled := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")

	orphaned := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	partitioned := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	awaitingBind := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	rowless := uuid.MustParse("44444444-4444-4444-4444-444444444444")

	spy := &rehomedSpy{
		vms: map[uuid.UUID]store.VM{
			// Pinned to a node row that no longer exists (force-deleted).
			orphaned: {ID: orphaned, Name: "orphan-1", PinnedNodeID: &deadHome},
			// Pinned to a node the control plane cannot currently reach: it may be
			// dead, in which case this reporter holds the last copy.
			partitioned: {ID: partitioned, Name: "partitioned-1", PinnedNodeID: &unreachableHome},
			// Unscheduled: nobody owns it, so nobody else can be running it.
			awaitingBind: {ID: awaitingBind, Name: "awaiting-1"},
		},
		nodes: map[uuid.UUID]store.Node{
			unreachableHome: {ID: unreachableHome, Status: store.NodeStatusUnreachable},
			unscheduled:     {ID: unscheduled, Status: store.NodeStatusReady},
		},
	}
	reports := vmReportsFor(orphaned, partitioned, awaitingBind, rowless)
	outcome := runVMReports(t, newQuietHandler(), spy, reporter, reports)

	if len(outcome.vmTombstones) != 0 {
		t.Errorf("tombstones = %v, want none (no proven live home for any of them)", outcome.vmTombstones)
	}
}

// TestHeartbeat_RehomedTeardownRechecksThePinAgainstAFreshRead asserts the
// decision is made against the row read here, not against the batch placement
// gate: a bind landing between the two must not order the guest it just placed
// on this very node destroyed.
func TestHeartbeat_RehomedTeardownRechecksThePinAgainstAFreshRead(t *testing.T) {
	reporter := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	vmID := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	// The gate says "not pinned here" (a stale read); the row says otherwise.
	spy := &rehomedSpy{
		vms:   map[uuid.UUID]store.VM{vmID: {ID: vmID, Name: "web-1", PinnedNodeID: &reporter}},
		nodes: map[uuid.UUID]store.Node{reporter: {ID: reporter, Status: store.NodeStatusReady}},
	}
	outcome := runVMReports(t, newQuietHandler(), spy, reporter, vmReportsFor(vmID))

	if len(outcome.vmTombstones) != 0 {
		t.Errorf("tombstones = %v, want none (the fresh read pins the VM to the reporter)", outcome.vmTombstones)
	}
}

// TestHeartbeat_RehomedTeardownSkipsOnAFailedRead asserts a read failure emits
// nothing and does not stop the rest of the batch: uncertainty resolves toward
// leaving the guest running.
func TestHeartbeat_RehomedTeardownSkipsOnAFailedRead(t *testing.T) {
	reporter := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	home := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	unreadableHome := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")

	unreadableVM := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	unreadableNode := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	rehomed := uuid.MustParse("33333333-3333-3333-3333-333333333333")

	spy := &rehomedSpy{
		vms: map[uuid.UUID]store.VM{
			unreadableVM:   {ID: unreadableVM, Name: "unreadable-1", PinnedNodeID: &home},
			unreadableNode: {ID: unreadableNode, Name: "unreadable-2", PinnedNodeID: &unreadableHome},
			rehomed:        {ID: rehomed, Name: "web-1", PinnedNodeID: &home},
		},
		vmErr:   map[uuid.UUID]error{unreadableVM: errors.New("boom")},
		nodeErr: map[uuid.UUID]error{unreadableHome: errors.New("boom")},
		nodes:   map[uuid.UUID]store.Node{home: {ID: home, Status: store.NodeStatusReady}},
	}
	buf := &bytes.Buffer{}
	outcome := runVMReports(t, newCapturingHandler(buf), spy, reporter,
		vmReportsFor(unreadableVM, unreadableNode, rehomed))

	want := []vmTombstone{{VMID: rehomed, VMName: "web-1"}}
	if diff := cmp.Diff(want, outcome.vmTombstones); diff != "" {
		t.Errorf("tombstones mismatch (-want +got):\n%s", diff)
	}
	if !strings.Contains(buf.String(), "rehomed lookup failed") {
		t.Errorf("logs = %q, want a line reporting the failed lookup", buf.String())
	}
}

// TestHeartbeat_RehomedTeardownSkipsAdmittedReports asserts a report the
// placement gate admitted is never resolved at all. That gate admits the
// pinned node AND an active migration's target, so this is what keeps a
// migration in flight from being told to destroy the incoming guest.
func TestHeartbeat_RehomedTeardownSkipsAdmittedReports(t *testing.T) {
	target := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	source := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	vmID := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	// Pinned to the source (the pin stays there until cutover) but admitted on
	// the target by the migration arm of the placement gate. Read as a re-homed
	// report this is indistinguishable from a stale duplicate - the pin names
	// another live node - so a tombstone here would order the incoming guest
	// destroyed mid-migration.
	spy := &rehomedSpy{
		pinned: []uuid.UUID{vmID},
		vms:    map[uuid.UUID]store.VM{vmID: {ID: vmID, Name: "web-1", PinnedNodeID: &source}},
		nodes:  map[uuid.UUID]store.Node{source: {ID: source, Status: store.NodeStatusReady}},
	}
	outcome := runVMReports(t, newQuietHandler(), spy, target, vmReportsFor(vmID))

	if len(outcome.vmTombstones) != 0 {
		t.Errorf("tombstones = %v, want none (the placement gate admitted this report)", outcome.vmTombstones)
	}
}
