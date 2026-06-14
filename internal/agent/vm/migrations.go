// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/migration"
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

// Migrations returns the agent's in-memory migration record store.
func (m *Manager) Migrations() *migration.Store { return m.migrations }

// IncomingSpec parameterizes target-side migration preparation.
//
// DiskSizeBytes carries the source disk's virtual size (the VMSpec boot
// disk size_gib resolved to bytes by the handler). ExpectedSize is the
// progress-UX estimate from MigrationIncomingRequest.expected_size_bytes.
// The destination disk is created at max(ExpectedSize, DiskSizeBytes); see
// StartIncoming for why under-sizing must be avoided.
type IncomingSpec struct {
	MigrationID    uuid.UUID
	VMUUID         uuid.UUID
	VMName         string
	VCPUs          int
	MemoryMB       int
	PoolName       string
	Architecture   qemu.Architecture
	Mode           string
	ExpectedSize   int64
	DiskSizeBytes  int64
	SourceIdentity string
	BindHost       string
}

// IncomingResult is returned to the CP and relayed to the source.
type IncomingResult struct {
	ListenEndpoint string
	AuthToken      string
}

// StartIncoming prepares this node to receive a migrated VM: reserve a
// port, materialize server TLS creds, create the destination disk, adopt a
// stopped Migrated VM, and start a writable TLS NBD server pinned to the
// source identity. Synchronous; returns the endpoint + token for the source.
//
// Cleanup-on-failure is intentionally minimal: cleanup releases the reserved
// port. If AdoptForMigration succeeds but a later step fails the adopted VM
// record and any partial destination disk are left behind; for this slice
// that is acceptable - the CP re-drives the migration as failed and the
// partial is GC'd. No migration record is stored until every step succeeds,
// so an early failure leaves nothing in m.migrations to delete.
func (m *Manager) StartIncoming(ctx context.Context, s IncomingSpec) (IncomingResult, error) {
	port, err := m.migPorts.Reserve()
	if err != nil {
		// %w so the handler can errors.Is(ErrNoFreePort) and surface a
		// retryable capacity condition rather than a hard failure.
		return IncomingResult{}, fmt.Errorf("reserve migration port: %w", err)
	}
	cleanup := func() { m.migPorts.Release(port) }

	credsDir := filepath.Join(m.stateDir, "migrations", s.MigrationID.String(), "tls")
	if err := qemu.MaterializeMigrationCreds(credsDir, qemu.MigrationTLSServer, m.tlsCA, m.tlsCert, m.tlsKey); err != nil {
		cleanup()
		return IncomingResult{}, err
	}

	v, err := m.AdoptForMigration(AdoptSpec{
		UUID: s.VMUUID, Name: s.VMName, VCPUs: s.VCPUs, MemoryMB: s.MemoryMB,
		PoolName: s.PoolName, Architecture: s.Architecture,
	})
	if err != nil {
		cleanup()
		return IncomingResult{}, err
	}

	// Destination disk virtual size must be >= the source disk's virtual
	// size, otherwise the source-side `qemu-img convert -n` push fails (it
	// cannot write past the end of a smaller target). Over-sizing is safe:
	// the resulting qcow2 just has extra unused virtual space and still
	// boots. So pick the larger of the source disk virtual size and the
	// progress-UX estimate. CreateDisk ensures the per-VM parent dir exists.
	virtualBytes := s.DiskSizeBytes
	if s.ExpectedSize > virtualBytes {
		virtualBytes = s.ExpectedSize
	}
	if err := m.migCreateDisk(ctx, v.DiskPath, virtualBytes); err != nil {
		cleanup()
		return IncomingResult{}, err
	}

	token := s.MigrationID.String()
	nbdPid, err := m.migSpawnNBD(ctx, qemu.QemuNBDServerArgs(qemu.QemuNBDServerSpec{
		CredsDir: credsDir, SourceIdentity: s.SourceIdentity, BindHost: s.BindHost,
		Port: port, Export: token, DiskPath: v.DiskPath,
	}))
	if err != nil {
		cleanup()
		return IncomingResult{}, err
	}

	endpoint := net.JoinHostPort(s.BindHost, strconv.Itoa(port))
	m.migrations.Put(&migration.Record{
		MigrationID: s.MigrationID, VMID: s.VMUUID, VMName: s.VMName,
		Role: migration.RoleTarget, Mode: migration.Mode(s.Mode), Phase: migration.PhaseSetup,
		Port: port, NBDPid: nbdPid, ListenEndpt: endpoint, AuthToken: token,
		CredsDir: credsDir, CreatedAt: time.Now().UTC(),
	})
	return IncomingResult{ListenEndpoint: endpoint, AuthToken: token}, nil
}
