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

// IncomingResult is returned to the agent-API handler, which serializes it
// back to the CP; the CP then relays the endpoint + token to the source.
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
	nbdPid, err := m.migSpawnNBD(ctx, qemu.NBDServerArgs(qemu.NBDServerSpec{
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

// OutgoingSpec parameterizes source-side migration.
type OutgoingSpec struct {
	MigrationID    uuid.UUID
	VMUUID         uuid.UUID
	VMName         string
	Mode           string
	TargetEndpoint string
	TargetIdentity string
	AuthToken      string
}

// StartOutgoing kicks off the source side: mint a vm.migrate task, record a
// source migration, and run the disk push asynchronously. Returns the task
// immediately (202 semantics). Every failure path leaves the VM on this node
// (fail-safe) and marks the migration failed.
func (m *Manager) StartOutgoing(ctx context.Context, s OutgoingSpec) (*AgentTask, error) {
	v, err := m.Get(s.VMUUID)
	if err != nil {
		return nil, fmt.Errorf("vm %s: %v", s.VMUUID, err)
	}
	task := m.tasks.Create(TaskKindVMMigrate, s.VMUUID)
	m.migrations.Put(&migration.Record{
		MigrationID: s.MigrationID, VMID: s.VMUUID, VMName: s.VMName,
		Role: migration.RoleSource, Mode: migration.Mode(s.Mode), Phase: migration.PhaseSetup,
		PeerEndpoint: s.TargetEndpoint, AuthToken: s.AuthToken, AgentTaskID: task.ID,
		CreatedAt: time.Now().UTC(),
	})
	// Detach from the request context: the push outlives the HTTP call (202
	// semantics), so a request-scoped cancel must not abort the migration.
	// #nosec G118 -- the disk push intentionally outlives the 202 request (fire-and-converge); a cancelled HTTP request must not abort an in-flight guest migration. The CP re-drives on failure.
	go m.runOutgoing(context.Background(), task.ID, s, v.DiskPath)
	return task, nil
}

// runOutgoing executes the source-side push asynchronously: stop the VM if
// running, materialize client TLS creds, push the disk over NBD+TLS, and mark
// phases setup -> active -> completed. Any error marks the migration failed
// and leaves the VM on this node (fail-safe).
//
// KNOWN LIMITATION (2a): the push runs through m.migRunConvert, which wraps the
// blocking qemu.RunQemuImgConvert and returns no pid. Record.ConvertPid is
// therefore never set on the source, so CancelMigration cannot interrupt an
// in-flight push (its ConvertPid > 0 branch is dormant here). This is
// acceptable: cancel is best-effort, an uninterruptible push runs to completion
// or the migration fails, and either way the VM stays on the source.
func (m *Manager) runOutgoing(ctx context.Context, taskID uuid.UUID, s OutgoingSpec, srcDisk string) {
	fail := func(code, msg string) {
		m.migrations.Update(s.MigrationID, func(r *migration.Record) {
			r.Phase = migration.PhaseFailed
			r.ErrorMessage = msg
			r.CompletedAt = time.Now().UTC()
		})
		m.tasks.Update(taskID, func(t *AgentTask) {
			t.Status = TaskStatusFailed
			t.Error = &TaskError{Code: code, Message: msg}
		})
	}

	m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusRunning })

	// 1. Stop the VM if running (offline move).
	if v, err := m.Get(s.VMUUID); err == nil && v.Status == StatusRunning {
		if _, err := m.Stop(ctx, s.VMName); err != nil {
			fail("stop_failed", fmt.Sprintf("stop vm before migrate: %v", err))
			return
		}
		if err := m.waitStopped(ctx, s.VMUUID, 90*time.Second); err != nil {
			fail("stop_failed", fmt.Sprintf("wait vm stopped: %v", err))
			return
		}
	}
	m.migrations.Update(s.MigrationID, func(r *migration.Record) { r.Phase = migration.PhaseActive; r.StartedAt = time.Now().UTC() })

	// 2. Client TLS creds.
	credsDir := filepath.Join(m.stateDir, "migrations", s.MigrationID.String(), "tls")
	if err := qemu.MaterializeMigrationCreds(credsDir, qemu.MigrationTLSClient, m.tlsCA, m.tlsCert, m.tlsKey); err != nil {
		fail("tls_setup_failed", err.Error())
		return
	}
	m.migrations.Update(s.MigrationID, func(r *migration.Record) { r.CredsDir = credsDir })

	// 3. Push the disk over NBD+TLS.
	host, portStr, err := net.SplitHostPort(s.TargetEndpoint)
	if err != nil {
		fail("internal", fmt.Sprintf("bad target endpoint %q: %v", s.TargetEndpoint, err))
		return
	}
	port, _ := strconv.Atoi(portStr)
	if err := m.migRunConvert(ctx, qemu.ImgPushArgs(qemu.ImgPushSpec{
		CredsDir: credsDir, SourceDisk: srcDisk, TargetHost: host, TargetPort: port,
		TargetIdentity: s.TargetIdentity, Export: s.AuthToken,
	})); err != nil {
		fail("convergence_failed", fmt.Sprintf("disk push: %v", err))
		return
	}

	// 4. Done. The CP worker observes success, runs cutover, then starts on
	//    target and deletes this copy. We only mark completed here.
	m.migrations.Update(s.MigrationID, func(r *migration.Record) {
		r.Phase = migration.PhaseCompleted
		r.CompletedAt = time.Now().UTC()
	})
	m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusSuccess })
}

// waitStopped polls until the VM reaches StatusStopped or the timeout elapses.
func (m *Manager) waitStopped(ctx context.Context, id uuid.UUID, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		v, err := m.Get(id)
		if err == nil && v.Status == StatusStopped {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("vm %s not stopped within %s", id, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// MigrationView projects a record into the agent-API Migration fields the
// handler serializes.
type MigrationView struct {
	MigrationID      uuid.UUID
	VMUUID           uuid.UUID
	Role             string
	Mode             string
	Phase            string
	ProgressPercent  int
	BytesTotal       *int64
	BytesTransferred int64
	PeerEndpoint     *string
	ErrorMessage     *string
	CreatedAt        time.Time
}

// GetMigration returns the agent's current view of a migration.
func (m *Manager) GetMigration(id uuid.UUID) (MigrationView, bool) {
	rec, ok := m.migrations.Get(id)
	if !ok {
		return MigrationView{}, false
	}
	pct := 0
	if rec.Phase == migration.PhaseCompleted {
		pct = 100
	} else if rec.BytesTotal > 0 {
		pct = int(rec.BytesTransferred * 100 / rec.BytesTotal)
	}
	view := MigrationView{
		MigrationID: rec.MigrationID, VMUUID: rec.VMID, Role: string(rec.Role),
		Mode: string(rec.Mode), Phase: string(rec.Phase), ProgressPercent: pct,
		BytesTransferred: rec.BytesTransferred, CreatedAt: rec.CreatedAt,
	}
	if rec.BytesTotal > 0 {
		bt := rec.BytesTotal
		view.BytesTotal = &bt
	}
	if rec.PeerEndpoint != "" {
		pe := rec.PeerEndpoint
		view.PeerEndpoint = &pe
	} else if rec.ListenEndpt != "" {
		le := rec.ListenEndpt
		view.PeerEndpoint = &le
	}
	if rec.ErrorMessage != "" {
		em := rec.ErrorMessage
		view.ErrorMessage = &em
	}
	return view, true
}

// CancelMigration aborts a pre-cutover migration: kill child processes,
// release the port, and pin phase=cancelled. Best-effort and idempotent; a
// terminal migration is returned unchanged.
//
// On the source, an in-flight disk push cannot be interrupted (see
// runOutgoing's KNOWN LIMITATION: ConvertPid is never set), so the
// ConvertPid > 0 branch below is dormant for source records today. The push
// runs to completion or fails; the VM stays on source either way.
func (m *Manager) CancelMigration(id uuid.UUID) (MigrationView, bool) {
	rec, ok := m.migrations.Get(id)
	if !ok {
		return MigrationView{}, false
	}
	if !rec.Terminal() {
		if rec.NBDPid > 0 {
			_ = qemu.Kill(rec.NBDPid)
		}
		if rec.ConvertPid > 0 {
			_ = qemu.Kill(rec.ConvertPid)
		}
		if rec.Port > 0 {
			m.migPorts.Release(rec.Port)
		}
		m.migrations.Update(id, func(r *migration.Record) {
			r.Phase = migration.PhaseCancelled
			r.ErrorMessage = "cancelled"
			r.CompletedAt = time.Now().UTC()
		})
	}
	return m.GetMigration(id)
}
