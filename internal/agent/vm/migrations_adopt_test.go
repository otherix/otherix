// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/netfabric"
	"github.com/otherix/otherix/internal/agent/qemu"
	"github.com/otherix/otherix/internal/agent/state"
)

func TestAdoptForMigrationCreatesStoppedMigratedVM(t *testing.T) {
	m := newTestManager(t)

	id := uuid.New()
	v, err := m.AdoptForMigration(AdoptSpec{
		UUID:         id,
		Name:         "demo",
		VCPUs:        1,
		MemoryMib:    512,
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

// reopenManager builds a fresh Manager over m's state dir and default pool, so
// the VMs it holds come only from the meta.json replay a restarted agent runs.
func reopenManager(t *testing.T, m *Manager) *Manager {
	t.Helper()
	cfg, _, _ := newTestConfig(t)
	cfg.StatePath = m.stateDir
	m2, err := New(cfg, &netfabric.FakeFabric{}, discardLogger())
	if err != nil {
		t.Fatalf("New(reopen): %v", err)
	}
	if err := m2.AddPool(m.defaultTestPool(), m.defaultTestPoolRoot(t)); err != nil {
		t.Fatalf("AddPool(reopen): %v", err)
	}
	return m2
}

func TestAdoptForMigrationRecordsAdoptingMigration(t *testing.T) {
	m := newTestManager(t)
	id, mid := uuid.New(), uuid.New()
	v, err := m.AdoptForMigration(AdoptSpec{
		UUID: id, Name: "demo", VCPUs: 1, MemoryMib: 512,
		PoolName: m.defaultTestPool(), Architecture: "amd64", MigrationID: mid,
	})
	if err != nil {
		t.Fatalf("AdoptForMigration() error = %v", err)
	}
	if v.AdoptedBy != mid {
		t.Errorf("AdoptForMigration().AdoptedBy = %v, want %v", v.AdoptedBy, mid)
	}
	meta, err := state.ReadMeta(filepath.Join(m.stateDir, id.String()))
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if meta.AdoptedBy != mid {
		t.Errorf("meta.AdoptedBy = %v, want %v", meta.AdoptedBy, mid)
	}

	m2 := reopenManager(t, m)
	m2.mu.Lock()
	got := m2.vms[id]
	m2.mu.Unlock()
	if got == nil || got.AdoptedBy != mid {
		t.Errorf("replayed VM = %+v, want AdoptedBy %v", got, mid)
	}
}

// TestCreateDedupClaimsAdoptedVM pins that a create redelivered after a
// restart, which claims an adopted copy as the node's own, clears the record
// of the adopting migration in memory and on disk.
func TestCreateDedupClaimsAdoptedVM(t *testing.T) {
	m := newTestManager(t)
	id, mid := uuid.New(), uuid.New()
	if _, err := m.AdoptForMigration(AdoptSpec{
		UUID: id, Name: "demo", VCPUs: 1, MemoryMib: 512,
		PoolName: m.defaultTestPool(), Architecture: qemu.HostArch(), MigrationID: mid,
	}); err != nil {
		t.Fatalf("AdoptForMigration() error = %v", err)
	}

	m2 := reopenManager(t, m)
	task, err := m2.Create(t.Context(), CreateSpec{
		UUID: id, Name: "demo", VCPUs: 1, MemoryMib: 512,
		PoolName: m.defaultTestPool(), ImageURL: "https://example.test/ubuntu.img",
	})
	if err != nil || task == nil {
		t.Fatalf("Create(dup adopted) = %v, %v, want idempotent task", task, err)
	}
	m2.mu.Lock()
	got := m2.vms[id].AdoptedBy
	m2.mu.Unlock()
	if got != uuid.Nil {
		t.Errorf("AdoptedBy after create dedup = %v, want nil", got)
	}
	meta, err := state.ReadMeta(filepath.Join(m.stateDir, id.String()))
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if meta.AdoptedBy != uuid.Nil {
		t.Errorf("meta.AdoptedBy after create dedup = %v, want nil", meta.AdoptedBy)
	}
}

func TestStartIncomingRecordsAdoptingMigration(t *testing.T) {
	for _, mode := range []string{"offline", "live"} {
		t.Run(mode, func(t *testing.T) {
			m := newTestManager(t)
			m.migCreateDisk = func(context.Context, string, int64) error { return nil }
			m.migSpawnNBD = func(context.Context, []string) (*qemu.NBDServer, error) {
				return &qemu.NBDServer{Pid: 4321}, nil
			}
			m.migLaunchIncoming = func(context.Context, *VM, qemu.LiveIncomingSpec) error { return nil }
			m.migDialQMP = func(string) (qemu.LiveSourceConn, error) { return &fakeLiveConn{}, nil }

			vmID, migID := uuid.New(), uuid.New()
			if _, err := m.StartIncoming(context.Background(), IncomingSpec{
				MigrationID: migID, VMUUID: vmID, VMName: "demo", VCPUs: 1, MemoryMib: 512,
				PoolName: m.defaultTestPool(), Architecture: "amd64", Mode: mode,
				ExpectedSize: 1 << 30, DiskSizeBytes: 1 << 30,
				SourceIdentity: "CN=node-src", BindHost: "10.0.0.2",
			}); err != nil {
				t.Fatalf("StartIncoming(%s) error = %v", mode, err)
			}
			m.mu.Lock()
			got := m.vms[vmID].AdoptedBy
			m.mu.Unlock()
			if got != migID {
				t.Errorf("StartIncoming(%s) AdoptedBy = %v, want %v", mode, got, migID)
			}
		})
	}
}
