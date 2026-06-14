// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/qemu"
)

// AdoptSpec describes a VM whose disk is being migrated in from another
// node. The target adopts a stopped, Migrated record plus a destination
// disk path; the disk file itself is created and filled by the migration
// flow (Task 8), not by this method.
type AdoptSpec struct {
	UUID         uuid.UUID
	Name         string
	VCPUs        int
	MemoryMB     int
	PoolName     string
	Architecture qemu.Architecture
}

// AdoptForMigration registers a stopped, Migrated VM on this (target) node
// and returns it. It computes the destination disk path inside the named
// pool, mirroring the create flow, and persists meta.json so a later start
// boots the copied disk without cloning. It does not touch QEMU or create
// the disk file. The pool must already be registered (reconciled from
// desired state); an unknown pool returns ErrPoolUnknown.
func (m *Manager) AdoptForMigration(spec AdoptSpec) (*VM, error) {
	m.poolsMu.RLock()
	p, ok := m.pools[spec.PoolName]
	m.poolsMu.RUnlock()
	if !ok {
		return nil, ErrPoolUnknown
	}

	now := time.Now().UTC()
	disk, qmp, console, pid := m.vmPaths(p.root, spec.UUID)
	v := &VM{
		ID:            spec.UUID,
		Name:          spec.Name,
		VCPUs:         spec.VCPUs,
		MemoryMB:      spec.MemoryMB,
		PoolName:      spec.PoolName,
		Architecture:  spec.Architecture,
		Status:        StatusStopped,
		CreatedAt:     now,
		UpdatedAt:     now,
		DiskPath:      disk,
		QMPSocket:     qmp,
		ConsoleSocket: console,
		PIDFile:       pid,
		Migrated:      true,
	}

	m.mu.Lock()
	if _, exists := m.vms[v.ID]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("vm %s already present", v.ID)
	}
	m.vms[v.ID] = v
	m.mu.Unlock()

	if err := m.persistVM(v.ID); err != nil {
		m.mu.Lock()
		delete(m.vms, v.ID)
		m.mu.Unlock()
		return nil, fmt.Errorf("persist adopted vm: %v", err)
	}
	return v, nil
}
