// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/netfabric"
	"github.com/otherix/otherix/internal/agent/state"
)

// sampleNIC returns a deterministic NIC for the tests.
func sampleNIC() netfabric.NIC {
	return netfabric.NIC{
		ID:          uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		Bridge:      "br-test",
		MAC:         "52:54:00:12:34:56",
		Model:       "virtio",
		MTU:         1500,
		DeviceOrder: 0,
	}
}

func TestMaterialiseNICs_CreatesThenAttachesInOrder(t *testing.T) {
	fake := &netfabric.FakeFabric{}
	m := &Manager{fabric: fake, log: discardLogger()}

	nic := sampleNIC()
	if err := m.materialiseNICs([]netfabric.NIC{nic}); err != nil {
		t.Fatalf("materialiseNICs = %v, want nil", err)
	}

	wantTap := nic.TapName()
	if got := fake.CreateTapCalls; len(got) != 1 || got[0].Name != wantTap || got[0].MTU != nic.MTU {
		t.Errorf("CreateTapCalls = %v, want one call {%s, %d}", got, wantTap, nic.MTU)
	}
	if got := fake.AttachTapCalls; len(got) != 1 || got[0].Tap != wantTap || got[0].Bridge != nic.Bridge {
		t.Errorf("AttachTapCalls = %v, want one call {%s, %s}", got, wantTap, nic.Bridge)
	}
}

func TestMaterialiseNICs_CreateTapError_RollsBack(t *testing.T) {
	fake := &netfabric.FakeFabric{Errs: map[string]error{"CreateTap": errors.New("boom")}}
	m := &Manager{fabric: fake, log: discardLogger()}

	nic := sampleNIC()
	if err := m.materialiseNICs([]netfabric.NIC{nic}); err == nil {
		t.Fatalf("materialiseNICs = nil, want error")
	}
	// CreateTap failed on the first NIC, so nothing was successfully
	// created and no rollback DeleteTap is expected.
	if len(fake.DeleteTapCalls) != 0 {
		t.Errorf("DeleteTapCalls = %v, want none (nothing created)", fake.DeleteTapCalls)
	}
}

func TestMaterialiseNICs_AttachTapError_RollsBackCreatedTap(t *testing.T) {
	fake := &netfabric.FakeFabric{Errs: map[string]error{"AttachTap": errors.New("boom")}}
	m := &Manager{fabric: fake, log: discardLogger()}

	nic := sampleNIC()
	if err := m.materialiseNICs([]netfabric.NIC{nic}); err == nil {
		t.Fatalf("materialiseNICs = nil, want error")
	}
	// The tap was created before AttachTap failed; rollback must delete it.
	wantTap := nic.TapName()
	if got := fake.DeleteTapCalls; len(got) != 1 || got[0] != wantTap {
		t.Errorf("DeleteTapCalls = %v, want one best-effort delete of %s", got, wantTap)
	}
}

func TestTeardownNICs_DeletesEachTap(t *testing.T) {
	fake := &netfabric.FakeFabric{Errs: map[string]error{"DeleteTap": errors.New("ignored")}}
	m := &Manager{fabric: fake, log: discardLogger()}

	nic := sampleNIC()
	// teardownNICs is best-effort and must not abort on DeleteTap error.
	m.teardownNICs([]netfabric.NIC{nic})

	wantTap := nic.TapName()
	if got := fake.DeleteTapCalls; len(got) != 1 || got[0] != wantTap {
		t.Errorf("DeleteTapCalls = %v, want one delete of %s", got, wantTap)
	}
}

// TestSweepOrphanTaps_DeletesOrphansWhenReplayComplete is the happy path: with a
// complete replay, a tap the fabric reports that belongs to no replayed VM is a
// genuine leak and is reclaimed, while a tap owned by a replayed VM is kept.
func TestSweepOrphanTaps_DeletesOrphansWhenReplayComplete(t *testing.T) {
	keptNIC := sampleNIC()
	keptTap := keptNIC.TapName()
	orphanTap := "ot-deadbeef-orphan"

	fake := &netfabric.FakeFabric{ListTapsResult: []string{keptTap, orphanTap}}
	vmID := uuid.New()
	m := &Manager{
		fabric: fake,
		log:    discardLogger(),
		vms:    map[uuid.UUID]*VM{vmID: {ID: vmID, NICs: []netfabric.NIC{keptNIC}}},
	}

	m.sweepOrphanTaps(true)

	if got := fake.DeleteTapCalls; len(got) != 1 || got[0] != orphanTap {
		t.Errorf("DeleteTapCalls = %v, want exactly [%s] (kept tap %s must survive)", got, orphanTap, keptTap)
	}
}

// TestSweepOrphanTaps_SkipsAllDeletesWhenReplayIncomplete is the Fix-Risk-Gate
// guard: when replay was INCOMPLETE (ScanState skipped a corrupt/missing-meta VM
// dir), the expected-tap set may miss a live VM, so the destructive sweep must do
// NOTHING - even a tap that looks orphaned could belong to the skipped-but-live
// VM. Fail toward inaction: leak the tap, never sever a running VM's networking.
func TestSweepOrphanTaps_SkipsAllDeletesWhenReplayIncomplete(t *testing.T) {
	orphanTap := "ot-deadbeef-orphan"
	fake := &netfabric.FakeFabric{ListTapsResult: []string{orphanTap}}
	m := &Manager{fabric: fake, log: discardLogger(), vms: map[uuid.UUID]*VM{}}

	m.sweepOrphanTaps(false)

	if len(fake.DeleteTapCalls) != 0 {
		t.Errorf("DeleteTapCalls = %v, want none (incomplete replay must not delete any tap)", fake.DeleteTapCalls)
	}
	// It must not even enumerate taps: the decision is made before listing.
	if fake.ListTapsCalls != 0 {
		t.Errorf("ListTapsCalls = %d, want 0 (sweep short-circuits on incomplete replay)", fake.ListTapsCalls)
	}
}

func TestNICMetaRoundTrip(t *testing.T) {
	nic := sampleNIC()
	want := []netfabric.NIC{nic}

	metas := nicsToMeta(want)
	wantMeta := []state.NICMeta{{
		ID:          nic.ID,
		Bridge:      nic.Bridge,
		MAC:         nic.MAC,
		Model:       nic.Model,
		MTU:         nic.MTU,
		DeviceOrder: nic.DeviceOrder,
		TapName:     nic.TapName(),
	}}
	if diff := cmp.Diff(wantMeta, metas); diff != "" {
		t.Errorf("nicsToMeta mismatch (-want +got):\n%s", diff)
	}

	got := metaToNICs(metas)
	if diff := cmp.Diff(want, got, cmpopts.EquateComparable(netip.Addr{})); diff != "" {
		t.Errorf("round-trip mismatch (-want +got):\n%s", diff)
	}
}
