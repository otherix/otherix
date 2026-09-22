// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package migration

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestStorePutGetUpdate(t *testing.T) {
	s := NewStore()
	id := uuid.New()
	now := time.Unix(1000, 0).UTC()

	s.Put(&Record{
		MigrationID: id,
		VMID:        uuid.New(),
		VMName:      "demo",
		Role:        RoleTarget,
		Mode:        ModeOffline,
		Phase:       PhaseSetup,
		Port:        49152,
		CreatedAt:   now,
	})

	got, ok := s.Get(id)
	if !ok {
		t.Fatalf("Get(%s) not found", id)
	}
	if got.Phase != PhaseSetup || got.Role != RoleTarget {
		t.Errorf("Get() phase/role = %v/%v, want setup/target", got.Phase, got.Role)
	}

	// Update mutates under lock; the returned snapshot reflects it.
	ok = s.Update(id, func(r *Record) {
		r.Phase = PhaseActive
		r.BytesTransferred = 42
	})
	if !ok {
		t.Fatalf("Update(%s) = false, want true", id)
	}
	got, _ = s.Get(id)
	if got.Phase != PhaseActive || got.BytesTransferred != 42 {
		t.Errorf("after Update phase/bytes = %v/%d, want active/42", got.Phase, got.BytesTransferred)
	}

	// Snapshot is a copy: mutating it does not affect the store.
	got.Phase = PhaseFailed
	again, _ := s.Get(id)
	if again.Phase != PhaseActive {
		t.Errorf("Get() returned shared pointer; store phase mutated to %v", again.Phase)
	}

	s.Delete(id)
	if _, ok := s.Get(id); ok {
		t.Errorf("Get(%s) after Delete = found, want absent", id)
	}
}

func TestRecord_LiveFields_RoundTrip(t *testing.T) {
	s := NewStore()
	id := uuid.New()
	s.Put(&Record{MigrationID: id, Role: RoleTarget, Mode: ModeLive, NBDPort: 49153, BlockJobID: "mirror-disk0"})
	got, ok := s.Get(id)
	if !ok {
		t.Fatal("Get() not found")
	}
	if got.NBDPort != 49153 || got.BlockJobID != "mirror-disk0" {
		t.Errorf("live fields = {NBDPort:%d BlockJobID:%q}, want {49153 mirror-disk0}", got.NBDPort, got.BlockJobID)
	}
}

func TestStoreUpdateMissing(t *testing.T) {
	s := NewStore()
	if s.Update(uuid.New(), func(*Record) {}) {
		t.Errorf("Update(missing) = true, want false")
	}
}

func TestHasActiveForVM(t *testing.T) {
	sourceVM := uuid.New()
	targetVM := uuid.New()
	doneVM := uuid.New()

	s := NewStore()
	s.Put(&Record{MigrationID: uuid.New(), VMID: sourceVM, Role: RoleSource, Phase: PhaseActive})
	s.Put(&Record{MigrationID: uuid.New(), VMID: targetVM, Role: RoleTarget, Phase: PhaseSetup})
	s.Put(&Record{MigrationID: uuid.New(), VMID: doneVM, Role: RoleSource, Phase: PhaseCompleted})

	tests := []struct {
		name string
		vmID uuid.UUID
		want bool
	}{
		{name: "non-terminal source record", vmID: sourceVM, want: true},
		{name: "non-terminal target record", vmID: targetVM, want: true},
		{name: "terminal record", vmID: doneVM, want: false},
		{name: "unrelated vm", vmID: uuid.New(), want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.HasActiveForVM(tc.vmID); got != tc.want {
				t.Errorf("HasActiveForVM(%v) = %v, want %v", tc.vmID, got, tc.want)
			}
		})
	}
}

// TestTakeIncoming: the take returns the NON-terminal target record for a VM and
// removes it, never a terminal one (its finalizer already stopped the server and
// released the ports), honours offlineOnly, and ignores source records. A VM can
// hold a terminal target record from an earlier failed attempt next to the live
// one; the take must pick the live one every time, not by map order.
func TestTakeIncoming(t *testing.T) {
	vmID := uuid.New()
	tests := []struct {
		name        string
		recs        []Record
		offlineOnly bool
		wantMig     uuid.UUID // uuid.Nil: nothing taken
	}{
		{
			name:    "non-terminal offline target",
			recs:    []Record{{MigrationID: uuid.MustParse("00000000-0000-0000-0000-000000000001"), VMID: vmID, Role: RoleTarget, Mode: ModeOffline, Phase: PhaseSetup}},
			wantMig: uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		},
		{
			name: "terminal record next to the live one",
			recs: []Record{
				{MigrationID: uuid.MustParse("00000000-0000-0000-0000-000000000002"), VMID: vmID, Role: RoleTarget, Mode: ModeLive, Phase: PhaseFailed},
				{MigrationID: uuid.MustParse("00000000-0000-0000-0000-000000000003"), VMID: vmID, Role: RoleTarget, Mode: ModeOffline, Phase: PhaseSetup},
			},
			wantMig: uuid.MustParse("00000000-0000-0000-0000-000000000003"),
		},
		{
			name: "only a terminal record",
			recs: []Record{{MigrationID: uuid.New(), VMID: vmID, Role: RoleTarget, Mode: ModeOffline, Phase: PhaseCancelled}},
		},
		{
			name:        "offlineOnly skips a live target",
			recs:        []Record{{MigrationID: uuid.New(), VMID: vmID, Role: RoleTarget, Mode: ModeLive, Phase: PhaseActive}},
			offlineOnly: true,
		},
		{
			name:        "offlineOnly takes a non-terminal target with no mode",
			recs:        []Record{{MigrationID: uuid.MustParse("00000000-0000-0000-0000-000000000004"), VMID: vmID, Role: RoleTarget, Phase: PhaseSetup}},
			offlineOnly: true,
			wantMig:     uuid.MustParse("00000000-0000-0000-0000-000000000004"),
		},
		{
			name: "source records are never taken",
			recs: []Record{{MigrationID: uuid.New(), VMID: vmID, Role: RoleSource, Mode: ModeOffline, Phase: PhaseActive}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Repeat so map iteration order cannot make a wrong pick pass by luck.
			for i := 0; i < 50; i++ {
				s := NewStore()
				for j := range tc.recs {
					s.Put(&tc.recs[j])
				}
				rec, ok := s.TakeIncoming(vmID, tc.offlineOnly)
				if tc.wantMig == uuid.Nil {
					if ok {
						t.Fatalf("TakeIncoming = %s, want nothing", rec.MigrationID)
					}
					continue
				}
				if !ok || rec.MigrationID != tc.wantMig {
					t.Fatalf("TakeIncoming = (%s, %v), want (%s, true)", rec.MigrationID, ok, tc.wantMig)
				}
				if _, again := s.TakeIncoming(vmID, tc.offlineOnly); again {
					t.Fatalf("second TakeIncoming returned a record, want the first take to remove it")
				}
			}
		})
	}
}

// TestIncoming returns the non-terminal target record without removing it.
func TestIncoming(t *testing.T) {
	s := NewStore()
	vmID := uuid.New()
	live := uuid.New()
	s.Put(&Record{MigrationID: uuid.New(), VMID: vmID, Role: RoleTarget, Mode: ModeOffline, Phase: PhaseFailed})
	s.Put(&Record{MigrationID: live, VMID: vmID, Role: RoleTarget, Mode: ModeOffline, Phase: PhaseSetup})

	rec, ok := s.Incoming(vmID)
	if !ok || rec.MigrationID != live {
		t.Fatalf("Incoming = (%s, %v), want (%s, true)", rec.MigrationID, ok, live)
	}
	if _, still := s.Get(live); !still {
		t.Errorf("Incoming removed the record, want it left in place")
	}
	if _, ok := s.Incoming(uuid.New()); ok {
		t.Errorf("Incoming(unknown vm) = true, want false")
	}
}
