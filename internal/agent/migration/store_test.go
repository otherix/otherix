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

func TestTakeTargetByVM(t *testing.T) {
	s := NewStore()
	targetVM := uuid.New()
	sourceVM := uuid.New()
	targetMig := uuid.New()
	sourceMig := uuid.New()

	s.Put(&Record{MigrationID: targetMig, VMID: targetVM, Role: RoleTarget, Port: 49152, NBDPid: 4242})
	s.Put(&Record{MigrationID: sourceMig, VMID: sourceVM, Role: RoleSource})

	rec, ok := s.TakeTargetByVM(targetVM)
	if !ok {
		t.Fatalf("TakeTargetByVM(%s) = false, want true", targetVM)
	}
	if rec.MigrationID != targetMig || rec.Port != 49152 || rec.NBDPid != 4242 {
		t.Errorf("TakeTargetByVM returned %+v, want target record (mig=%s port=49152 pid=4242)", rec, targetMig)
	}
	// Removed on take: a second call finds nothing.
	if _, ok := s.TakeTargetByVM(targetVM); ok {
		t.Errorf("TakeTargetByVM(%s) second call = true, want false (record removed)", targetVM)
	}
	if _, ok := s.Get(targetMig); ok {
		t.Errorf("Get(%s) after take = found, want absent", targetMig)
	}

	// A source-role record is never returned (and stays put).
	if _, ok := s.TakeTargetByVM(sourceVM); ok {
		t.Errorf("TakeTargetByVM(%s) = true for source-role record, want false", sourceVM)
	}
	if _, ok := s.Get(sourceMig); !ok {
		t.Errorf("source record removed by TakeTargetByVM; want left in place")
	}
}
