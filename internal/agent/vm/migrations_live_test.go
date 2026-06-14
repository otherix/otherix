// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitalocean/go-qemu/qmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/migration"
	"github.com/otherix/otherix/internal/agent/qemu"
)

// fakeLiveConn is a no-op qemu.LiveSourceConn used to exercise the manager's
// live-migration wiring without a real QEMU. Only the verbs the manager calls
// directly (SetupLiveIncoming, cancelLive) need to behave; the source run is
// exercised through the injected migRunLiveSource seam, so the bulk of the
// interface is satisfied with do-nothing stubs.
type fakeLiveConn struct {
	migrateCancelled bool
	blockJobCancels  []string
}

func (f *fakeLiveConn) ObjectAddTLSCreds(id, dir, endpoint string) error { return nil }
func (f *fakeLiveConn) ObjectAddAuthz(id, identity string) error         { return nil }
func (f *fakeLiveConn) BlockdevAddNBD(nodeName, host string, port int, export, tlsCreds, tlsHostname string) error {
	return nil
}
func (f *fakeLiveConn) BlockdevMirror(jobID, device, target string) error { return nil }
func (f *fakeLiveConn) NBDServerStart(host string, port int, tlsCreds, tlsAuthz string) error {
	return nil
}
func (f *fakeLiveConn) BlockExportAdd(id, nodeName, name string, writable bool) error { return nil }
func (f *fakeLiveConn) MigrateSetCapabilities(caps map[string]bool) error             { return nil }
func (f *fakeLiveConn) MigrateSetParameters(p qemu.LiveParams) error                  { return nil }
func (f *fakeLiveConn) Migrate(uri string) error                                      { return nil }
func (f *fakeLiveConn) MigrateIncoming(uri string) error                              { return nil }
func (f *fakeLiveConn) BlockJobCancel(device string, force bool) error {
	f.blockJobCancels = append(f.blockJobCancels, device)
	return nil
}
func (f *fakeLiveConn) MigrateContinue() error { return nil }
func (f *fakeLiveConn) MigrateCancel() error {
	f.migrateCancelled = true
	return nil
}
func (f *fakeLiveConn) BlockdevDel(nodeName string) error       { return nil }
func (f *fakeLiveConn) QueryMigrate() (qemu.MigrateInfo, error) { return qemu.MigrateInfo{}, nil }
func (f *fakeLiveConn) QueryBlockJobs() ([]qemu.BlockJobInfo, error) {
	return []qemu.BlockJobInfo{}, nil
}

func (f *fakeLiveConn) Events(ctx context.Context) (<-chan qmp.Event, error) {
	return nil, nil
}
func (f *fakeLiveConn) Close() error { return nil }

func TestStartIncoming_LiveReservesPairAndReturnsBothEndpoints(t *testing.T) {
	m := newTestManager(t)

	var launched bool
	m.migLaunchIncoming = func(ctx context.Context, v *VM, ls qemu.LiveIncomingSpec) error {
		launched = true
		return nil
	}
	m.migDialQMP = func(socket string) (qemu.LiveSourceConn, error) { return &fakeLiveConn{}, nil }
	m.migCreateDisk = func(ctx context.Context, path string, virtualBytes int64) error { return nil }

	migID := uuid.New()
	res, err := m.StartIncoming(context.Background(), IncomingSpec{
		MigrationID:    migID,
		VMUUID:         uuid.New(),
		VMName:         "demo",
		VCPUs:          1,
		MemoryMB:       512,
		PoolName:       m.defaultTestPool(),
		Architecture:   "amd64",
		Mode:           "live",
		ExpectedSize:   1 << 30,
		DiskSizeBytes:  1 << 30,
		SourceIdentity: "CN=node-src",
		BindHost:       "10.0.0.2",
	})
	if err != nil {
		t.Fatalf("StartIncoming(live) error = %v", err)
	}
	if !launched {
		t.Errorf("migLaunchIncoming not called on live incoming")
	}
	if res.ListenEndpoint == "" || res.NBDEndpoint == "" {
		t.Errorf("live result endpoints empty: %+v", res)
	}
	if res.ListenEndpoint == res.NBDEndpoint {
		t.Errorf("RAM and NBD endpoints must differ, both = %q", res.ListenEndpoint)
	}
	rec, ok := m.Migrations().Get(migID)
	if !ok {
		t.Fatalf("no migration record stored")
	}
	if rec.Mode != migration.ModeLive {
		t.Errorf("record Mode = %q, want live", rec.Mode)
	}
	if rec.Role != migration.RoleTarget {
		t.Errorf("record Role = %q, want target", rec.Role)
	}
	if rec.NBDPort == 0 {
		t.Errorf("record NBDPort not set")
	}
}

// TestStartIncoming_OfflineUnchanged confirms the offline path does NOT route
// through the live branch: migLaunchIncoming must not be called. Revert-confirm
// for the mode routing.
func TestStartIncoming_OfflineUnchanged(t *testing.T) {
	m := newTestManager(t)
	m.migCreateDisk = func(ctx context.Context, path string, virtualBytes int64) error { return nil }
	m.migSpawnNBD = func(ctx context.Context, args []string) (int, error) { return 4321, nil }
	m.migLaunchIncoming = func(ctx context.Context, v *VM, ls qemu.LiveIncomingSpec) error {
		t.Fatalf("migLaunchIncoming called on offline migration")
		return nil
	}

	migID := uuid.New()
	res, err := m.StartIncoming(context.Background(), IncomingSpec{
		MigrationID:    migID,
		VMUUID:         uuid.New(),
		VMName:         "demo",
		VCPUs:          1,
		MemoryMB:       512,
		PoolName:       m.defaultTestPool(),
		Architecture:   "amd64",
		Mode:           "offline",
		ExpectedSize:   1 << 30,
		SourceIdentity: "CN=node-src",
		BindHost:       "10.0.0.2",
	})
	if err != nil {
		t.Fatalf("StartIncoming(offline) error = %v", err)
	}
	if res.NBDEndpoint != "" {
		t.Errorf("offline result must not carry NBDEndpoint, got %q", res.NBDEndpoint)
	}
	rec, _ := m.Migrations().Get(migID)
	if rec.Mode != migration.ModeOffline {
		t.Errorf("record Mode = %q, want offline", rec.Mode)
	}
}

func TestRunOutgoingLive_NoPoweroff_DrivesToCompleted(t *testing.T) {
	m := newTestManager(t)
	v := m.seedRunningVM(t, "demo")

	var ranLive bool
	var gotSpec qemu.LiveSourceSpec
	m.migRunLiveSource = func(ctx context.Context, conn qemu.LiveSourceConn, spec qemu.LiveSourceSpec, report func(qemu.LiveProgress)) error {
		ranLive = true
		gotSpec = spec
		return nil
	}
	m.migDialQMP = func(socket string) (qemu.LiveSourceConn, error) { return &fakeLiveConn{}, nil }

	migID := uuid.New()
	task, err := m.StartOutgoing(context.Background(), OutgoingSpec{
		MigrationID:    migID,
		VMUUID:         v.ID,
		VMName:         v.Name,
		Mode:           "live",
		TargetEndpoint: "10.0.0.2:49152",
		NBDEndpoint:    "10.0.0.2:49153",
		TargetIdentity: "node-tgt.agents.otherix.local",
		AuthToken:      migID.String(),
	})
	if err != nil {
		t.Fatalf("StartOutgoing(live) error = %v", err)
	}

	waitPhase(t, m, migID, "completed")
	if !ranLive {
		t.Errorf("migRunLiveSource was not invoked")
	}

	// The blockdev-mirror device= must be the boot disk's BlockBackend name.
	// The boot disk launches as the first `-drive if=virtio`, so QEMU names it
	// virtio0. The wrong value (virtio-disk0) is what the smoke caught: real
	// qemu replied "Cannot find device='virtio-disk0'". This is the teeth the
	// prior tests lacked.
	if gotSpec.SrcDiskNode != "virtio0" {
		t.Errorf("spec.SrcDiskNode = %q, want %q", gotSpec.SrcDiskNode, "virtio0")
	}

	// Assert the raw persisted status: live migration must NOT power off the
	// source guest. (m.Get probes the pidfile, which is absent under the unit
	// fake, so read the in-map status directly to isolate the no-poweroff
	// invariant from the liveness probe.)
	m.mu.Lock()
	rawStatus := m.vms[v.ID].Status
	m.mu.Unlock()
	if rawStatus != StatusRunning {
		t.Errorf("live source VM status = %q, want running (must NOT be powered off)", rawStatus)
	}

	tk := m.tasks.Get(task.ID)
	if tk == nil || tk.Status != TaskStatusSuccess {
		t.Errorf("task = %v, want status success", tk)
	}
}

// failingSetupConn is a fakeLiveConn whose NBDServerStart fails, driving
// SetupLiveIncoming to return an error so startIncomingLive takes the failure
// path after the VM has been adopted and the -incoming qemu launched.
type failingSetupConn struct {
	fakeLiveConn
}

func (f *failingSetupConn) NBDServerStart(host string, port int, tlsCreds, tlsAuthz string) error {
	return errors.New("nbd-server-start: Address already in use")
}

// TestStartIncoming_LiveFailedPrepDoesNotPoisonRetry is the teeth test for the
// rollback: a failed incoming prep must leave NOTHING behind (no adopted VM, no
// leaked qemu, ports released) so the CP's retry starts fresh. Without the full
// cleanup, the second call fails with "vm ... already present" because the
// adopted VM record survives the first failure.
func TestStartIncoming_LiveFailedPrepDoesNotPoisonRetry(t *testing.T) {
	m := newTestManager(t)

	// migLaunchIncoming is a no-op (no real qemu); killQEMU is then a quiet
	// no-op on the missing pidfile when the rollback fires.
	m.migLaunchIncoming = func(ctx context.Context, v *VM, ls qemu.LiveIncomingSpec) error { return nil }
	m.migCreateDisk = func(ctx context.Context, path string, virtualBytes int64) error { return nil }

	// First dial yields a conn whose setup fails; second dial succeeds.
	dials := 0
	m.migDialQMP = func(socket string) (qemu.LiveSourceConn, error) {
		dials++
		if dials == 1 {
			return &failingSetupConn{}, nil
		}
		return &fakeLiveConn{}, nil
	}

	migID := uuid.New()
	vmUUID := uuid.New()
	spec := IncomingSpec{
		MigrationID:    migID,
		VMUUID:         vmUUID,
		VMName:         "demo",
		VCPUs:          1,
		MemoryMB:       512,
		PoolName:       m.defaultTestPool(),
		Architecture:   "amd64",
		Mode:           "live",
		ExpectedSize:   1 << 30,
		DiskSizeBytes:  1 << 30,
		SourceIdentity: "CN=node-src",
		BindHost:       "10.0.0.2",
	}

	// First attempt fails in SetupLiveIncoming.
	if _, err := m.startIncomingLive(context.Background(), spec); err == nil {
		t.Fatalf("first startIncomingLive: want error, got nil")
	}

	// Rollback must have removed the adopted VM record entirely.
	if _, err := m.Get(vmUUID); !errors.Is(err, ErrNotFound) {
		t.Errorf("after failed prep, Get(%s) = %v, want ErrNotFound (VM record must be rolled back)", vmUUID, err)
	}
	// No migration record should have been stored on the failure path.
	if _, ok := m.Migrations().Get(migID); ok {
		t.Errorf("migration record stored despite failed prep")
	}

	// Second attempt with the same spec must SUCCEED, proving the rollback
	// unpoisoned the retry (no "vm ... already present").
	res, err := m.startIncomingLive(context.Background(), spec)
	if err != nil {
		t.Fatalf("second startIncomingLive: want success, got %v", err)
	}
	if res.ListenEndpoint == "" || res.NBDEndpoint == "" {
		t.Errorf("retry result endpoints empty: %+v", res)
	}
	rec, ok := m.Migrations().Get(migID)
	if !ok {
		t.Fatalf("no migration record stored after successful retry")
	}
	if rec.Role != migration.RoleTarget {
		t.Errorf("record Role = %q, want target", rec.Role)
	}
}

// stubTargetConn is a no-op qemu.LiveTargetConn used to exercise the
// target-side resume wiring without a real QEMU. RunLiveTarget itself is
// injected through the migRunLiveTarget seam, so every method is a do-nothing
// stub.
type stubTargetConn struct{}

func (stubTargetConn) Events(ctx context.Context) (<-chan qmp.Event, error) { return nil, nil }
func (stubTargetConn) BlockExportDel(id string) error                       { return nil }
func (stubTargetConn) NBDServerStop() error                                 { return nil }
func (stubTargetConn) Cont() error                                          { return nil }
func (stubTargetConn) Close() error                                         { return nil }

// waitStatus polls the raw in-map VM status (not m.Get, which probes the
// pidfile absent under the unit fake) until it reaches want or the deadline.
func waitStatus(t *testing.T, m *Manager, id uuid.UUID, want Status) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		got := m.vms[id].Status
		m.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	m.mu.Lock()
	got := m.vms[id].Status
	m.mu.Unlock()
	t.Fatalf("status never reached %q; last = %q", want, got)
}

// TestStartIncoming_LiveResumeDrivesToRunning is the success-path teeth for the
// target-side resume: once startIncomingLive completes setup it spawns a tracked
// task that runs RunLiveTarget (injected here to return nil). On success the VM
// must reach StatusRunning and the reserved migration port pair must be released
// (a fresh ReservePair reclaiming them must succeed even after exhausting the
// range minus one).
func TestStartIncoming_LiveResumeDrivesToRunning(t *testing.T) {
	m := newTestManager(t)
	m.migLaunchIncoming = func(ctx context.Context, v *VM, ls qemu.LiveIncomingSpec) error { return nil }
	m.migDialQMP = func(socket string) (qemu.LiveSourceConn, error) { return &fakeLiveConn{}, nil }
	m.migCreateDisk = func(ctx context.Context, path string, virtualBytes int64) error { return nil }
	m.migDialQMPTarget = func(socket string) (qemu.LiveTargetConn, error) { return stubTargetConn{}, nil }
	var ranTarget bool
	m.migRunLiveTarget = func(ctx context.Context, conn qemu.LiveTargetConn, spec qemu.LiveTargetSpec) error {
		ranTarget = true
		return nil
	}

	// Narrow the migration ingress to exactly one (RAM, NBD) pair so the
	// post-resume ReservePair below has teeth: startIncomingLive reserves the
	// only pair, and a fresh reservation can succeed afterwards ONLY if
	// runIncomingResume released it. A regression dropping that release would
	// leave the range exhausted and the final ReservePair would fail.
	m.migPorts = migration.NewPortAllocator(49152, 49153)

	migID := uuid.New()
	vmUUID := uuid.New()
	res, err := m.startIncomingLive(context.Background(), IncomingSpec{
		MigrationID:    migID,
		VMUUID:         vmUUID,
		VMName:         "demo",
		VCPUs:          1,
		MemoryMB:       512,
		PoolName:       m.defaultTestPool(),
		Architecture:   "amd64",
		Mode:           "live",
		ExpectedSize:   1 << 30,
		DiskSizeBytes:  1 << 30,
		SourceIdentity: "CN=node-src",
		BindHost:       "10.0.0.2",
	})
	if err != nil {
		t.Fatalf("startIncomingLive error = %v", err)
	}
	if res.ListenEndpoint == "" || res.NBDEndpoint == "" {
		t.Fatalf("result endpoints empty: %+v", res)
	}

	waitStatus(t, m, vmUUID, StatusRunning)
	if !ranTarget {
		t.Errorf("migRunLiveTarget was not invoked")
	}

	waitPhase(t, m, migID, "completed")

	// The reserved port pair must be released on success. The range holds
	// exactly one pair (set above), so this fresh reservation succeeds only
	// because runIncomingResume released the pair startIncomingLive held; if the
	// release regressed, the range stays exhausted and ReservePair returns
	// ErrNoFreePort.
	if _, _, err := m.migPorts.ReservePair(); err != nil {
		t.Errorf("ports not released after resume: %v", err)
	}
}

// TestStartIncoming_LiveResumeFailureMarksVMFailed is the failure-path teeth:
// when RunLiveTarget returns an error there is no fall-back to the source (it is
// already released), so the target VM must be marked StatusFailed and the record
// failed.
func TestStartIncoming_LiveResumeFailureMarksVMFailed(t *testing.T) {
	m := newTestManager(t)
	m.migLaunchIncoming = func(ctx context.Context, v *VM, ls qemu.LiveIncomingSpec) error { return nil }
	m.migDialQMP = func(socket string) (qemu.LiveSourceConn, error) { return &fakeLiveConn{}, nil }
	m.migCreateDisk = func(ctx context.Context, path string, virtualBytes int64) error { return nil }
	m.migDialQMPTarget = func(socket string) (qemu.LiveTargetConn, error) { return stubTargetConn{}, nil }
	m.migRunLiveTarget = func(ctx context.Context, conn qemu.LiveTargetConn, spec qemu.LiveTargetSpec) error {
		return errors.New("incoming did not converge")
	}

	migID := uuid.New()
	vmUUID := uuid.New()
	if _, err := m.startIncomingLive(context.Background(), IncomingSpec{
		MigrationID:    migID,
		VMUUID:         vmUUID,
		VMName:         "demo",
		VCPUs:          1,
		MemoryMB:       512,
		PoolName:       m.defaultTestPool(),
		Architecture:   "amd64",
		Mode:           "live",
		ExpectedSize:   1 << 30,
		DiskSizeBytes:  1 << 30,
		SourceIdentity: "CN=node-src",
		BindHost:       "10.0.0.2",
	}); err != nil {
		t.Fatalf("startIncomingLive error = %v", err)
	}

	waitStatus(t, m, vmUUID, StatusFailed)
	waitPhase(t, m, migID, "failed")
}

func TestCancelLive_SetsCancelledAndReleasesPorts(t *testing.T) {
	m := newTestManager(t)
	v := m.seedRunningVM(t, "demo")

	fc := &fakeLiveConn{}
	m.migDialQMP = func(socket string) (qemu.LiveSourceConn, error) { return fc, nil }

	ram, nbd, err := m.migPorts.ReservePair()
	if err != nil {
		t.Fatalf("ReservePair: %v", err)
	}
	migID := uuid.New()
	m.migrations.Put(&migration.Record{
		MigrationID: migID, VMID: v.ID, VMName: v.Name,
		Role: migration.RoleSource, Mode: migration.ModeLive, Phase: migration.PhaseActive,
		Port: ram, NBDPort: nbd, BlockJobID: "mirror-disk0",
	})

	view, ok := m.CancelMigration(migID)
	if !ok {
		t.Fatalf("CancelMigration returned ok=false")
	}
	if view.Phase != string(migration.PhaseCancelled) {
		t.Errorf("phase = %q, want cancelled", view.Phase)
	}
	if !fc.migrateCancelled {
		t.Errorf("MigrateCancel not issued on live cancel")
	}
	// Ports must be released: a fresh ReservePair should now succeed reclaiming them.
	if _, _, err := m.migPorts.ReservePair(); err != nil {
		t.Errorf("ports not released after cancel: %v", err)
	}
}
