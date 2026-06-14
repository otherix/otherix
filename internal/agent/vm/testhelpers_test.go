// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"testing"

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
