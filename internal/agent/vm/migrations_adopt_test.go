// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

func TestAdoptForMigrationCreatesStoppedMigratedVM(t *testing.T) {
	m := newTestManager(t)

	id := uuid.New()
	v, err := m.AdoptForMigration(AdoptSpec{
		UUID:         id,
		Name:         "demo",
		VCPUs:        1,
		MemoryMB:     512,
		PoolName:     m.defaultTestPool(),
		Architecture: "amd64",
	})
	if err != nil {
		t.Fatalf("AdoptForMigration() error = %v", err)
	}
	if v.Status != StatusStopped {
		t.Errorf("adopted status = %v, want stopped", v.Status)
	}
	if !v.Migrated {
		t.Errorf("adopted Migrated = false, want true")
	}
	wantDisk := filepath.Join(m.defaultTestPoolRoot(t), "vms", id.String(), "disk.qcow2")
	if v.DiskPath != wantDisk {
		t.Errorf("DiskPath = %q, want %q", v.DiskPath, wantDisk)
	}
	got, err := m.Get(id)
	if err != nil || got.ID != id {
		t.Errorf("Get(adopted) = %v,%v", got, err)
	}
}
