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

func TestStoreUpdateMissing(t *testing.T) {
	s := NewStore()
	if s.Update(uuid.New(), func(*Record) {}) {
		t.Errorf("Update(missing) = true, want false")
	}
}
