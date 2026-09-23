// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/migration"
	"github.com/otherix/otherix/internal/agent/qemu"
)

// TestReleaseIncoming_FreesBothPorts drives the cold-live release edge: a
// TARGET migration record carries a reserved (ram, nbd) pair. releaseIncoming
// must return BOTH ports to the allocator. With a 2-port range, leaking the NBD
// port leaves the pair exhausted and the next ReservePair fails ErrNoFreePort.
func TestReleaseIncoming_FreesBothPorts(t *testing.T) {
	m := newTestManager(t)
	m.migPorts = migration.NewPortAllocator(49152, 49153) // exactly one pair

	ram, nbd, err := m.migPorts.ReservePair()
	if err != nil {
		t.Fatalf("ReservePair: %v", err)
	}
	vmID := uuid.New()
	m.migrations.Put(&migration.Record{
		MigrationID: uuid.New(), VMID: vmID, Role: migration.RoleTarget,
		Mode: migration.ModeLive, Phase: migration.PhaseSetup, Port: ram, NBDPort: nbd,
	})

	m.releaseIncoming(vmID, false)

	if _, _, err := m.migPorts.ReservePair(); err != nil {
		t.Fatalf("ReservePair after release: %v (a port leaked)", err)
	}
}

// TestRemoveAdoptedVM_RemovesDiskDir confirms removeAdoptedVM removes the
// per-VM destination disk dir (pre-cutover rollback), not just the agent state
// dir. Safe because every caller is strictly pre-cutover.
func TestRemoveAdoptedVM_RemovesDiskDir(t *testing.T) {
	m := newTestManager(t)
	vmID := uuid.New()
	v, err := m.AdoptForMigration(AdoptSpec{
		UUID: vmID, Name: "ex", VCPUs: 1, MemoryMib: 512,
		PoolName: m.defaultTestPool(), Architecture: qemu.ArchAMD64,
	})
	if err != nil {
		t.Fatalf("AdoptForMigration: %v", err)
	}
	diskDir := filepath.Dir(v.DiskPath)
	if err := os.MkdirAll(diskDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(diskDir, "disk.qcow2"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	m.removeAdoptedVM(vmID)
	if _, err := os.Stat(diskDir); !os.IsNotExist(err) {
		t.Errorf("disk dir still present after removeAdoptedVM: stat err = %v", err)
	}
}

// TestCancelMigrationOfflineTargetIsIdempotent pins the SEQUENTIAL contract the
// control plane now depends on: an offline TARGET record is reaped exactly once
// however many times the cancel arrives, because a later call reads an already
// terminal record and short-circuits. Both the operator cancel path
// (propagateCancel) and the worker's terminal-outcome reap (reapTargetIncoming)
// call the agent, so a repeated cancel is ordinary traffic rather than an edge
// case.
//
// Scope, so this is not read as more than it is: what this drives is the
// `!rec.Terminal()` fast path, and it would still pass with the won-gate on the
// port release removed. That gate exists for the CONCURRENT case - two cancels
// both reading a non-terminal snapshot before either stamps - and the window
// between the snapshot read and the stamp has no seam to drive deterministically.
func TestCancelMigrationOfflineTargetIsIdempotent(t *testing.T) {
	m := newTestManager(t)
	migID := uuid.New()
	port, err := m.migPorts.Reserve()
	if err != nil {
		t.Fatalf("Reserve(): %v", err)
	}
	m.Migrations().Put(&migration.Record{
		MigrationID: migID, VMID: uuid.New(), VMName: "demo",
		Role: migration.RoleTarget, Mode: migration.ModeOffline,
		Phase: migration.PhaseSetup, Port: port,
	})

	if _, ok := m.CancelMigration(migID); !ok {
		t.Fatalf("first CancelMigration returned ok=false")
	}
	first, ok := m.Migrations().Get(migID)
	if !ok {
		t.Fatalf("record absent after first cancel")
	}
	if first.Phase != migration.PhaseCancelled {
		t.Fatalf("phase after first cancel = %q, want %q", first.Phase, migration.PhaseCancelled)
	}

	// A later migration takes the freed port. A second cancel of the ALREADY
	// terminal record must not hand this port back to the pool.
	reclaimed, err := m.migPorts.Reserve()
	if err != nil {
		t.Fatalf("Reserve() after cancel: %v", err)
	}
	if reclaimed != port {
		t.Fatalf("reclaimed port = %d, want the freed %d", reclaimed, port)
	}

	if _, ok := m.CancelMigration(migID); !ok {
		t.Fatalf("second CancelMigration returned ok=false")
	}
	second, ok := m.Migrations().Get(migID)
	if !ok {
		t.Fatalf("record absent after second cancel")
	}
	if !second.CompletedAt.Equal(first.CompletedAt) {
		t.Errorf("CompletedAt restamped by the second cancel: %v -> %v", first.CompletedAt, second.CompletedAt)
	}

	// The port the later migration holds must still be reserved: reserving again
	// has to yield a DIFFERENT port.
	next, err := m.migPorts.Reserve()
	if err != nil {
		t.Fatalf("Reserve() after second cancel: %v", err)
	}
	if next == reclaimed {
		t.Errorf("second cancel freed port %d, which a later migration holds", reclaimed)
	}
}

// TestReleaseIncomingNeverTouchesATerminalRecord: a terminal record's server was
// stopped and its ports released by whoever stamped it. Taking it again would
// signal a pid that may have been reused and free a port another migration now
// holds, so the release leaves it exactly as it is.
func TestReleaseIncomingNeverTouchesATerminalRecord(t *testing.T) {
	m := newTestManager(t)
	var stopped []int
	m.migStopNBD = func(srv *qemu.NBDServer, _ time.Duration) error {
		stopped = append(stopped, srv.Pid)
		return nil
	}
	port, err := m.migPorts.Reserve()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	vmID, migID := uuid.New(), uuid.New()
	m.Migrations().Put(&migration.Record{
		MigrationID: migID, VMID: vmID, Role: migration.RoleTarget,
		Mode: migration.ModeOffline, Phase: migration.PhaseCancelled,
		Port: port, NBD: &qemu.NBDServer{Pid: 77},
	})

	m.releaseIncoming(vmID, false)

	if len(stopped) != 0 {
		t.Errorf("stopped %v, want no stop for a terminal record", stopped)
	}
	if _, ok := m.Migrations().Get(migID); !ok {
		t.Errorf("terminal record removed, want it kept for GetMigration")
	}
	if p, err := m.migPorts.Reserve(); err == nil && p == port {
		t.Errorf("port %d reservable again, want it still held (released only by its finalizer)", port)
	}
}

// TestReleaseIncomingStopsTheLiveServerNotTheStaleOne: with a terminal record
// from an earlier attempt next to the in-flight one, the in-flight server is
// stopped and the stale pid is never signalled.
func TestReleaseIncomingStopsTheLiveServerNotTheStaleOne(t *testing.T) {
	for i := 0; i < 50; i++ {
		m := newTestManager(t)
		var stopped []int
		m.migStopNBD = func(srv *qemu.NBDServer, _ time.Duration) error {
			stopped = append(stopped, srv.Pid)
			return nil
		}
		vmID := uuid.New()
		m.Migrations().Put(&migration.Record{
			MigrationID: uuid.New(), VMID: vmID, Role: migration.RoleTarget,
			Mode: migration.ModeLive, Phase: migration.PhaseFailed, NBD: &qemu.NBDServer{Pid: 1},
		})
		m.Migrations().Put(&migration.Record{
			MigrationID: uuid.New(), VMID: vmID, Role: migration.RoleTarget,
			Mode: migration.ModeOffline, Phase: migration.PhaseSetup, NBD: &qemu.NBDServer{Pid: 2},
		})

		m.releaseIncoming(vmID, false)

		if len(stopped) != 1 || stopped[0] != 2 {
			t.Fatalf("iteration %d: stopped %v, want exactly [2]", i, stopped)
		}
	}
}

// TestReleaseIncomingKeepsEverythingWhenTheStopFails: a server that could not be
// stopped still holds its disk and its port, so the release puts the record back
// and keeps the port reserved for the next attempt; the attempt after a
// successful stop then frees both.
func TestReleaseIncomingKeepsEverythingWhenTheStopFails(t *testing.T) {
	m := newTestManager(t)
	m.migPorts = migration.NewPortAllocator(49152, 49152) // exactly one port
	stopErr := errors.New("qemu-nbd did not exit")
	var stopped []int
	m.migStopNBD = func(srv *qemu.NBDServer, _ time.Duration) error {
		stopped = append(stopped, srv.Pid)
		return stopErr
	}
	port, err := m.migPorts.Reserve()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	vmID, migID := uuid.New(), uuid.New()
	m.Migrations().Put(&migration.Record{
		MigrationID: migID, VMID: vmID, Role: migration.RoleTarget,
		Mode: migration.ModeOffline, Phase: migration.PhaseSetup,
		Port: port, NBD: &qemu.NBDServer{Pid: 55},
	})

	m.releaseIncoming(vmID, true)

	if len(stopped) != 1 {
		t.Fatalf("stopped %v, want one attempt", stopped)
	}
	if !m.HasActiveMigration(vmID) {
		t.Errorf("HasActiveMigration = false after a failed stop, want the record put back")
	}
	if p, err := m.migPorts.Reserve(); err == nil {
		t.Errorf("Reserve() = %d after a failed stop, want the port still held by the running server", p)
	}

	stopErr = nil
	m.releaseIncoming(vmID, true)

	if len(stopped) != 2 || stopped[1] != 55 {
		t.Errorf("stopped %v, want the retry to stop pid 55", stopped)
	}
	if m.HasActiveMigration(vmID) {
		t.Errorf("HasActiveMigration = true after a successful stop, want the record dropped")
	}
	if p, err := m.migPorts.Reserve(); err != nil || p != port {
		t.Errorf("Reserve() = (%d, %v) after a successful stop, want (%d, nil)", p, err, port)
	}
}

// TestReleaseIncomingAfterARealCancelLeavesALaterMigrationsPort drives the real
// sequence rather than writing a terminal record by hand: the cancel stops the
// server and frees the port, a later migration reserves that port, and a release
// arriving afterwards must neither stop anything nor free the port again.
func TestReleaseIncomingAfterARealCancelLeavesALaterMigrationsPort(t *testing.T) {
	m, spy := NewManagerForSeamTest(t)
	m.migPorts = migration.NewPortAllocator(49152, 49152) // exactly one port
	vmID, migID := uuid.New(), uuid.New()
	if _, err := m.StartIncoming(context.Background(), IncomingSpec{
		MigrationID: migID, VMUUID: vmID, VMName: "cold", VCPUs: 1, MemoryMib: 512,
		PoolName: m.defaultTestPool(), Architecture: qemu.ArchAMD64, Mode: "offline",
		DiskSizeBytes: 1 << 30, SourceIdentity: "CN=node-src", BindHost: "10.0.0.2",
	}); err != nil {
		t.Fatalf("StartIncoming: %v", err)
	}

	if _, ok := m.CancelMigration(migID); !ok {
		t.Fatalf("CancelMigration returned ok=false")
	}
	afterCancel := len(spy.Pids())
	later, err := m.migPorts.Reserve()
	if err != nil {
		t.Fatalf("Reserve() after cancel: %v, want the freed port", err)
	}

	m.releaseIncoming(vmID, false)

	if got := spy.Pids(); len(got) != afterCancel {
		t.Errorf("stops after the release = %v, want no stop beyond the cancel's %d", got, afterCancel)
	}
	if p, err := m.migPorts.Reserve(); err == nil {
		t.Errorf("Reserve() = %d after the release, want port %d still held by the later migration", p, later)
	}
}

// offlineIncomingSpec is a minimal offline incoming spec for vmID.
func offlineIncomingSpec(m *Manager, migID, vmID uuid.UUID) IncomingSpec {
	return IncomingSpec{
		MigrationID: migID, VMUUID: vmID, VMName: "cold", VCPUs: 1, MemoryMib: 512,
		PoolName: m.defaultTestPool(), Architecture: qemu.ArchAMD64, Mode: "offline",
		DiskSizeBytes: 1 << 30, SourceIdentity: "CN=node-src", BindHost: "10.0.0.2",
	}
}

// vmDiskDir is the per-VM disk dir an adopt of vmID in the default test pool uses.
func vmDiskDir(t *testing.T, m *Manager, vmID uuid.UUID) string {
	t.Helper()
	root, err := m.poolRoot(m.defaultTestPool())
	if err != nil {
		t.Fatalf("poolRoot: %v", err)
	}
	return filepath.Join(root, "vms", vmID.String())
}

// plantDisk writes a disk file with known bytes into the per-VM disk dir, as an
// older copy whose record is gone would leave it.
func plantDisk(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	disk := filepath.Join(dir, "disk.qcow2")
	if err := os.WriteFile(disk, []byte("older copy"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return disk
}

// assertDiskUntouched fails unless disk still holds the bytes plantDisk wrote.
func assertDiskUntouched(t *testing.T, disk string) {
	t.Helper()
	got, err := os.ReadFile(disk)
	if err != nil || string(got) != "older copy" {
		t.Errorf("ReadFile(%s) = (%q, %v), want the planted bytes untouched", disk, got, err)
	}
}

// assertNoAdoptedRecord fails when vmID is still in memory or on disk under the
// agent state dir.
func assertNoAdoptedRecord(t *testing.T, m *Manager, vmID uuid.UUID) {
	t.Helper()
	if _, err := m.Get(vmID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(%s) = %v, want ErrNotFound", vmID, err)
	}
	if _, err := os.Stat(filepath.Join(m.stateDir, vmID.String())); !os.IsNotExist(err) {
		t.Errorf("state dir of %s: stat err = %v, want not exist", vmID, err)
	}
}

// TestStartIncomingOfflineRefusesAnExistingDiskDir: a disk dir the call did not
// create is refused and left exactly as it was, and the adopt and the port are
// rolled back.
func TestStartIncomingOfflineRefusesAnExistingDiskDir(t *testing.T) {
	m, _ := NewManagerForSeamTest(t)
	m.migPorts = migration.NewPortAllocator(49152, 49152) // exactly one port
	var created []string
	m.migCreateDisk = func(_ context.Context, path string, _ int64) error {
		created = append(created, path)
		return os.WriteFile(path, nil, 0o600)
	}
	vmID := uuid.New()
	disk := plantDisk(t, vmDiskDir(t, m, vmID))

	_, err := m.StartIncoming(context.Background(), offlineIncomingSpec(m, uuid.New(), vmID))

	if !errors.Is(err, ErrDiskDirExists) {
		t.Fatalf("StartIncoming(offline) = %v, want ErrDiskDirExists", err)
	}
	if len(created) != 0 {
		t.Errorf("migCreateDisk called with %v, want no disk created", created)
	}
	assertDiskUntouched(t, disk)
	assertNoAdoptedRecord(t, m, vmID)
	if _, err := m.migPorts.Reserve(); err != nil {
		t.Errorf("Reserve() after the refusal: %v, want the port released", err)
	}
}

// TestStartIncomingOfflineRollsBackAFailedAdopt: a failure after the disk dir
// was created removes that dir, the record and the port, so a retry of the same
// migration adopts again.
func TestStartIncomingOfflineRollsBackAFailedAdopt(t *testing.T) {
	m, _ := NewManagerForSeamTest(t)
	m.migPorts = migration.NewPortAllocator(49152, 49152) // exactly one port
	m.migCreateDisk = func(_ context.Context, path string, _ int64) error {
		return os.WriteFile(path, []byte("new"), 0o600)
	}
	spawn := m.migSpawnNBD
	m.migSpawnNBD = func(context.Context, []string) (*qemu.NBDServer, error) {
		return nil, errors.New("qemu-nbd failed to start")
	}
	m.migWaitNBDReady = func(context.Context, string) error { return nil }
	vmID, migID := uuid.New(), uuid.New()

	if _, err := m.StartIncoming(context.Background(), offlineIncomingSpec(m, migID, vmID)); err == nil {
		t.Fatalf("StartIncoming(offline) = nil, want the spawn error")
	}

	assertNoAdoptedRecord(t, m, vmID)
	if _, err := os.Stat(vmDiskDir(t, m, vmID)); !os.IsNotExist(err) {
		t.Errorf("disk dir after rollback: stat err = %v, want not exist", err)
	}
	if _, ok := m.Migrations().Get(migID); ok {
		t.Errorf("migration record stored despite the failure")
	}

	m.migSpawnNBD = spawn
	if _, err := m.StartIncoming(context.Background(), offlineIncomingSpec(m, migID, vmID)); err != nil {
		t.Errorf("retry StartIncoming(offline) = %v, want success", err)
	}
}

// TestStartIncomingOfflineKeepsDiskAndPortWhenTheNBDStopFails: a qemu-nbd that
// could not be stopped still holds the disk and the port, so the rollback drops
// only the record.
func TestStartIncomingOfflineKeepsDiskAndPortWhenTheNBDStopFails(t *testing.T) {
	m, _ := NewManagerForSeamTest(t)
	m.migPorts = migration.NewPortAllocator(49152, 49152) // exactly one port
	m.migCreateDisk = func(_ context.Context, path string, _ int64) error {
		return os.WriteFile(path, []byte("new"), 0o600)
	}
	m.migWaitNBDReady = func(context.Context, string) error { return errors.New("not listening") }
	m.migStopNBD = func(*qemu.NBDServer, time.Duration) error { return errors.New("qemu-nbd did not exit") }
	vmID := uuid.New()

	if _, err := m.StartIncoming(context.Background(), offlineIncomingSpec(m, uuid.New(), vmID)); err == nil {
		t.Fatalf("StartIncoming(offline) = nil, want the readiness error")
	}

	assertNoAdoptedRecord(t, m, vmID)
	if _, err := os.Stat(vmDiskDir(t, m, vmID)); err != nil {
		t.Errorf("disk dir after a failed stop: stat err = %v, want it kept", err)
	}
	if p, err := m.migPorts.Reserve(); err == nil {
		t.Errorf("Reserve() = %d after a failed stop, want the port still held", p)
	}
}
