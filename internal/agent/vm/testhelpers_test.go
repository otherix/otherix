// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/netfabric"
)

// newTestManager builds a Manager over a temp state dir with a single
// registered pool ("default"), mirroring the setup the other manager
// tests assemble inline (newTestConfig + New + AddPool). The registered
// pool name is reachable via Manager.defaultTestPool.
func newTestManager(t *testing.T) *Manager {
	t.Helper()
	cfg, poolRoot, poolName := newTestConfig(t)
	m, err := New(cfg, &netfabric.FakeFabric{}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.AddPool(poolName, poolRoot); err != nil {
		t.Fatalf("AddPool(%s): %v", poolName, err)
	}
	return m
}

// defaultTestPool returns the name of the pool newTestManager registers.
func (m *Manager) defaultTestPool() string {
	return "default"
}

// defaultTestPoolRoot returns the on-disk root of the pool newTestManager
// registers, read back from the live registry so tests can pin the per-VM
// path formula against the same root Create / AdoptForMigration derive from.
func (m *Manager) defaultTestPoolRoot(t *testing.T) string {
	t.Helper()
	for _, p := range m.ListPools() {
		if p.Name == m.defaultTestPool() {
			return p.Root
		}
	}
	t.Fatalf("default test pool %q not registered", m.defaultTestPool())
	return ""
}

// seedStoppedVM registers a stopped VM in the default test pool with a real
// (stub) disk file on disk, mirroring how Create / AdoptForMigration set the
// VM fields. The disk content is irrelevant to the migration push fake; only
// the path and its parent directory must exist.
func (m *Manager) seedStoppedVM(t *testing.T, name string) *VM {
	t.Helper()
	id := uuid.New()
	root := m.defaultTestPoolRoot(t)
	disk, qmp, console, pid := m.vmPaths(root, id)
	if err := os.MkdirAll(filepath.Dir(disk), 0o750); err != nil {
		t.Fatalf("mkdir disk parent: %v", err)
	}
	if err := os.WriteFile(disk, []byte("qcow2-stub"), 0o600); err != nil {
		t.Fatalf("write stub disk: %v", err)
	}
	now := time.Now().UTC()
	v := &VM{
		ID:            id,
		Name:          name,
		VCPUs:         1,
		MemoryMB:      512,
		PoolName:      m.defaultTestPool(),
		Status:        StatusStopped,
		CreatedAt:     now,
		UpdatedAt:     now,
		DiskPath:      disk,
		QMPSocket:     qmp,
		ConsoleSocket: console,
		PIDFile:       pid,
	}
	m.mu.Lock()
	m.vms[id] = v
	m.mu.Unlock()
	return v
}
