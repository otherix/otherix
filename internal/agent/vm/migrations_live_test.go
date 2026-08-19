// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitalocean/go-qemu/qmp"
	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/migration"
	"github.com/otherix/otherix/internal/agent/netfabric"
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
	// migrateStatus is what QueryMigrate reports. The zero value ("") is
	// interpreted as "completed" so a bare &fakeLiveConn{} models a healthy,
	// converged migration; a test exercising the post-migration convergence
	// guard sets a non-completed status (e.g. "active", "failed") explicitly.
	migrateStatus string
	// queryMigrateErr, when set, makes QueryMigrate fail (the guard treats a
	// query error the same as a non-completed status: keep the source).
	queryMigrateErr error
}

func (f *fakeLiveConn) ObjectAddTLSCreds(id, dir, endpoint string) error { return nil }
func (f *fakeLiveConn) ObjectAddAuthz(id, identity string) error         { return nil }
func (f *fakeLiveConn) ObjectDel(id string) error                        { return nil }
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
func (f *fakeLiveConn) BlockdevDel(nodeName string) error { return nil }
func (f *fakeLiveConn) QueryMigrate() (qemu.MigrateInfo, error) {
	if f.queryMigrateErr != nil {
		return qemu.MigrateInfo{}, f.queryMigrateErr
	}
	status := f.migrateStatus
	if status == "" {
		status = "completed"
	}
	return qemu.MigrateInfo{Status: status}, nil
}

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
		MemoryMib:      512,
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

// cidataBuild records one migBuildCidata invocation so the manifest test can
// assert the cidata is REBUILT locally (hostname + cloud-init bytes) rather than
// created empty + NBD-exported.
type cidataBuild struct {
	path        string
	hostname    string
	userData    []byte
	networkData []byte
}

// TestStartIncoming_LiveMultiDiskReplicatesManifest asserts the target builds a
// destination disk per manifest entry: the writable boot disk via migCreateDisk
// (created + exported), and the read-only cidata REBUILT locally via
// migBuildCidata (NOT via migCreateRawDisk, NOT NBD-exported). It records the
// cidata path on the VM, passes a per-disk LiveIncomingSpec to the launch (cidata
// entry carries empty Export/ExportID + ReadOnly true), and persists ONLY the
// writable boot export id on the migration record.
func TestStartIncoming_LiveMultiDiskReplicatesManifest(t *testing.T) {
	m := newTestManager(t)

	// Capture (path, virtualBytes) per create seam so the size guard and the
	// cidata-not-inflated invariant are GUARDED: the boot disk must be created
	// at max(DiskSizeBytes, ExpectedSize). The read-only cidata must NOT touch
	// migCreateRawDisk at all - it is rebuilt via migBuildCidata.
	qcowSizes := map[string]int64{}
	rawSizes := map[string]int64{}
	m.migCreateDisk = func(_ context.Context, path string, virtualBytes int64) error {
		qcowSizes[path] = virtualBytes
		return nil
	}
	m.migCreateRawDisk = func(_ context.Context, path string, virtualBytes int64) error {
		rawSizes[path] = virtualBytes
		return nil
	}
	var builds []cidataBuild
	m.migBuildCidata = func(path, hostname string, userData, networkData []byte) error {
		builds = append(builds, cidataBuild{path: path, hostname: hostname, userData: userData, networkData: networkData})
		return nil
	}
	var gotSpec qemu.LiveIncomingSpec
	m.migLaunchIncoming = func(_ context.Context, _ *VM, ls qemu.LiveIncomingSpec) error {
		gotSpec = ls
		return nil
	}
	m.migDialQMP = func(socket string) (qemu.LiveSourceConn, error) { return &fakeLiveConn{}, nil }

	migID := uuid.New()
	vmUUID := uuid.New()
	// ExpectedSize > DiskSizeBytes so the index-0 under-size guard is actually
	// exercised: the boot disk must be created at ExpectedSize (3 GiB), not the
	// 2 GiB manifest size.
	const (
		bootManifestSize = 2 << 30
		expectedSize     = 3 << 30
		cidataSize       = 10 << 20
	)
	res, err := m.startIncomingLive(context.Background(), IncomingSpec{
		MigrationID:    migID,
		VMUUID:         vmUUID,
		VMName:         "demo",
		VCPUs:          1,
		MemoryMib:      512,
		PoolName:       m.defaultTestPool(),
		Architecture:   "amd64",
		Mode:           "live",
		ExpectedSize:   expectedSize,
		DiskSizeBytes:  bootManifestSize,
		SourceIdentity: "CN=node-src",
		BindHost:       "10.0.0.2",
		UserData:       "#cloud-config\nusers: []\n",
		NetworkConfig:  "version: 2\n",
		Disks: []MigrationDisk{
			{Index: 0, SizeBytes: bootManifestSize, Format: "qcow2", ReadOnly: false},
			{Index: 1, SizeBytes: cidataSize, Format: "raw", ReadOnly: true},
		},
	})
	if err != nil {
		t.Fatalf("startIncomingLive error = %v", err)
	}
	if res.NBDEndpoint == "" {
		t.Fatalf("NBD endpoint empty: %+v", res)
	}

	v, err := m.Get(vmUUID)
	if err != nil {
		t.Fatalf("Get(vm) = %v", err)
	}
	cidataPath := filepath.Join(filepath.Dir(v.DiskPath), "cidata.iso")

	if len(qcowSizes) != 1 {
		t.Errorf("qcow creates = %v, want exactly one at %s", qcowSizes, v.DiskPath)
	}
	// Boot disk: created at max(DiskSizeBytes, ExpectedSize) = ExpectedSize.
	if got := qcowSizes[v.DiskPath]; got != int64(expectedSize) {
		t.Errorf("boot disk virtualBytes = %d, want %d (max(DiskSizeBytes, ExpectedSize))", got, int64(expectedSize))
	}
	// The read-only cidata is REBUILT, not created via migCreateRawDisk.
	if len(rawSizes) != 0 {
		t.Errorf("raw creates = %v, want none (cidata is rebuilt via migBuildCidata)", rawSizes)
	}
	// cidata rebuilt exactly once, with v.Name as hostname and the IncomingSpec
	// cloud-init bytes.
	if len(builds) != 1 {
		t.Fatalf("migBuildCidata calls = %d, want 1 (%v)", len(builds), builds)
	}
	wantBuild := cidataBuild{
		path:        cidataPath,
		hostname:    "demo",
		userData:    []byte("#cloud-config\nusers: []\n"),
		networkData: []byte("version: 2\n"),
	}
	if diff := cmp.Diff(wantBuild, builds[0], cmp.AllowUnexported(cidataBuild{})); diff != "" {
		t.Errorf("migBuildCidata call mismatch (-want +got):\n%s", diff)
	}
	if v.CidataPath != cidataPath {
		t.Errorf("v.CidataPath = %q, want %q", v.CidataPath, cidataPath)
	}

	wantDisks := []qemu.LiveIncomingDisk{
		{Node: "virtio0", Path: v.DiskPath, Export: migID.String() + "-0", ExportID: "exp0", Format: "qcow2", ReadOnly: false},
		{Node: "virtio1", Path: cidataPath, Format: "raw", ReadOnly: true},
	}
	if diff := cmp.Diff(wantDisks, gotSpec.Disks); diff != "" {
		t.Errorf("LiveIncomingSpec.Disks mismatch (-want +got):\n%s", diff)
	}

	rec, ok := m.Migrations().Get(migID)
	if !ok {
		t.Fatalf("no migration record stored")
	}
	// Only the writable boot export is persisted; the cidata is not exported.
	if diff := cmp.Diff([]string{"exp0"}, rec.ExportIDs); diff != "" {
		t.Errorf("record ExportIDs mismatch (-want +got):\n%s", diff)
	}
}

// TestStartIncoming_LiveUnsupportedDiskFormatFailsClosed asserts an
// unrecognized manifest disk format is rejected (fail-closed) rather than
// silently defaulting to raw, and that the failure runs cleanup: no qemu is
// launched and the reserved port pair is released.
func TestStartIncoming_LiveUnsupportedDiskFormatFailsClosed(t *testing.T) {
	m := newTestManager(t)

	launched := false
	m.migLaunchIncoming = func(_ context.Context, _ *VM, _ qemu.LiveIncomingSpec) error {
		launched = true
		return nil
	}
	m.migDialQMP = func(socket string) (qemu.LiveSourceConn, error) { return &fakeLiveConn{}, nil }
	m.migCreateDisk = func(_ context.Context, _ string, _ int64) error { return nil }
	m.migCreateRawDisk = func(_ context.Context, _ string, _ int64) error { return nil }

	migID := uuid.New()
	vmUUID := uuid.New()
	_, err := m.startIncomingLive(context.Background(), IncomingSpec{
		MigrationID:    migID,
		VMUUID:         vmUUID,
		VMName:         "demo",
		VCPUs:          1,
		MemoryMib:      512,
		PoolName:       m.defaultTestPool(),
		Architecture:   "amd64",
		Mode:           "live",
		ExpectedSize:   1 << 30,
		DiskSizeBytes:  1 << 30,
		SourceIdentity: "CN=node-src",
		BindHost:       "10.0.0.2",
		Disks: []MigrationDisk{
			{Index: 0, SizeBytes: 1 << 30, Format: "vmdk", ReadOnly: false},
		},
	})
	if err == nil {
		t.Fatalf("startIncomingLive with unsupported format: want error, got nil")
	}

	// Cleanup must have run: no qemu launched, VM record rolled back, no record
	// stored, and the reserved port pair released.
	if launched {
		t.Errorf("migLaunchIncoming called despite unsupported disk format")
	}
	if _, err := m.Get(vmUUID); !errors.Is(err, ErrNotFound) {
		t.Errorf("after format failure, Get(%s) = %v, want ErrNotFound", vmUUID, err)
	}
	if _, ok := m.Migrations().Get(migID); ok {
		t.Errorf("migration record stored despite unsupported disk format")
	}
	if _, _, err := m.migPorts.ReservePair(); err != nil {
		t.Errorf("ports not released after format failure: %v", err)
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
		MemoryMib:      512,
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
	if len(gotSpec.Disks) != 1 {
		t.Fatalf("spec.Disks len = %d, want 1 (boot disk only, no cidata)", len(gotSpec.Disks))
	}
	wantBoot := qemu.LiveSourceDisk{
		SrcNode: "virtio0", JobID: "mirror-disk0", NBDNode: "mirror-target0", Export: migID.String() + "-0",
	}
	if gotSpec.Disks[0] != wantBoot {
		t.Errorf("spec.Disks[0] = %+v, want %+v", gotSpec.Disks[0], wantBoot)
	}

	// On success the departed source VM is torn down (the guest is now on the
	// target). It must be gone from the manager - never left powered-off-but-
	// present (the old graceful-poweroff path) and never still present at all.
	m.mu.Lock()
	_, present := m.vms[v.ID]
	m.mu.Unlock()
	if present {
		t.Errorf("live source VM still present after success; want torn down (departed source)")
	}

	tk := m.tasks.Get(task.ID)
	if tk == nil || tk.Status != TaskStatusSuccess {
		t.Errorf("task = %v, want status success", tk)
	}
}

// TestRunOutgoingLive_WithCidata_MirrorsBootOnly asserts that even when the
// source VM carries a cidata disk (v.CidataPath != "") the outgoing spec
// mirrors ONLY the writable boot disk (virtio0 -> <token>-0). The read-only
// cidata disk is rebuilt read-only on the target, not transferred over NBD, so
// the source must never append a cidata mirror. The single boot export name
// must byte-match the target's only export <migrationID>-0 (s.AuthToken ==
// migrationID.String()).
func TestRunOutgoingLive_WithCidata_MirrorsBootOnly(t *testing.T) {
	m := newTestManager(t)
	v := m.seedRunningVM(t, "demo")
	m.mu.Lock()
	v.CidataPath = filepath.Join(filepath.Dir(v.DiskPath), "cidata.iso")
	m.mu.Unlock()

	var gotSpec qemu.LiveSourceSpec
	m.migRunLiveSource = func(ctx context.Context, conn qemu.LiveSourceConn, spec qemu.LiveSourceSpec, report func(qemu.LiveProgress)) error {
		gotSpec = spec
		return nil
	}
	m.migDialQMP = func(socket string) (qemu.LiveSourceConn, error) { return &fakeLiveConn{}, nil }

	migID := uuid.New()
	if _, err := m.StartOutgoing(context.Background(), OutgoingSpec{
		MigrationID:    migID,
		VMUUID:         v.ID,
		VMName:         v.Name,
		Mode:           "live",
		TargetEndpoint: "10.0.0.2:49152",
		NBDEndpoint:    "10.0.0.2:49153",
		TargetIdentity: "node-tgt.agents.otherix.local",
		AuthToken:      migID.String(),
	}); err != nil {
		t.Fatalf("StartOutgoing(live) error = %v", err)
	}

	waitPhase(t, m, migID, "completed")

	want := []qemu.LiveSourceDisk{
		{SrcNode: "virtio0", JobID: "mirror-disk0", NBDNode: "mirror-target0", Export: migID.String() + "-0"},
	}
	if diff := cmp.Diff(want, gotSpec.Disks); diff != "" {
		t.Errorf("spec.Disks mismatch (-want +got):\n%s", diff)
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
	// The successful second attempt spawns the fire-and-forget resume goroutine
	// (go runIncomingResume on context.Background()). Stub the target-side seams
	// so it drives deterministically to StatusRunning; the test waits for that
	// below so the goroutine finishes its persistVM write before t.TempDir
	// cleanup runs (otherwise the write races RemoveAll: "directory not empty").
	m.migDialQMPTarget = func(socket string) (qemu.LiveTargetConn, error) { return stubTargetConn{}, nil }
	m.migRunLiveTarget = func(ctx context.Context, conn qemu.LiveTargetConn, spec qemu.LiveTargetSpec) error {
		return nil
	}

	migID := uuid.New()
	vmUUID := uuid.New()
	spec := IncomingSpec{
		MigrationID:    migID,
		VMUUID:         vmUUID,
		VMName:         "demo",
		VCPUs:          1,
		MemoryMib:      512,
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

	// Wait for the spawned resume goroutine to finish (it drives the VM to
	// StatusRunning via the stubbed target seams) before the test returns, so
	// its persistVM write does not race t.TempDir's RemoveAll cleanup.
	waitStatus(t, m, vmUUID, StatusRunning)
}

// stubTargetConn is a no-op qemu.LiveTargetConn used to exercise the
// target-side resume wiring without a real QEMU. RunLiveTarget itself is
// injected through the migRunLiveTarget seam, so every method is a do-nothing
// stub.
type stubTargetConn struct{}

func (stubTargetConn) Events(ctx context.Context) (<-chan qmp.Event, error) { return nil, nil }
func (stubTargetConn) BlockExportDel(id string) error                       { return nil }
func (stubTargetConn) BlockExportDelHard(id string) error                   { return nil }
func (stubTargetConn) ObjectDel(id string) error                            { return nil }
func (stubTargetConn) NBDServerStop() error                                 { return nil }
func (stubTargetConn) Cont() error                                          { return nil }
func (stubTargetConn) AnnounceSelf(qemu.AnnounceParameters) error           { return nil }
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
	m.migCreateRawDisk = func(ctx context.Context, path string, virtualBytes int64) error { return nil }
	m.migBuildCidata = func(path, hostname string, userData, networkData []byte) error { return nil }
	m.migDialQMPTarget = func(socket string) (qemu.LiveTargetConn, error) { return stubTargetConn{}, nil }
	var (
		ranTarget bool
		gotSpec   qemu.LiveTargetSpec
	)
	m.migRunLiveTarget = func(ctx context.Context, conn qemu.LiveTargetConn, spec qemu.LiveTargetSpec) error {
		ranTarget = true
		gotSpec = spec
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
		MemoryMib:      512,
		PoolName:       m.defaultTestPool(),
		Architecture:   "amd64",
		Mode:           "live",
		ExpectedSize:   1 << 30,
		DiskSizeBytes:  1 << 30,
		SourceIdentity: "CN=node-src",
		BindHost:       "10.0.0.2",
		// Two-disk manifest (boot qcow2 + cidata raw read-only): only the
		// writable boot disk is exported, so the resume tears down exp0 only.
		Disks: []MigrationDisk{
			{Index: 0, SizeBytes: 1 << 30, Format: "qcow2"},
			{Index: 1, SizeBytes: 1 << 20, Format: "raw", ReadOnly: true},
		},
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
	// The resume must delete every WRITABLE export the record carries. The
	// read-only cidata is rebuilt locally, never exported, so only the boot
	// export (exp0) is torn down.
	if diff := cmp.Diff([]string{"exp0"}, gotSpec.ExportIDs); diff != "" {
		t.Errorf("LiveTargetSpec.ExportIDs mismatch (-want +got):\n%s", diff)
	}

	waitPhase(t, m, migID, "completed")

	// The phase is stamped "completed" at qemu-cont success, several best-effort
	// steps (announce/GARP/persist/mux) BEFORE runIncomingResume releases the
	// port pair, so waitPhase alone races the release and the reservation below
	// intermittently sees the range still exhausted. Await the detached resume
	// goroutine (resumeWG.Done fires after runIncomingResume returns, i.e. after
	// the release) so the release has run before the check.
	m.resumeWG.Wait()

	// The reserved port pair must be released on success. The range holds
	// exactly one pair (set above), so this fresh reservation succeeds only
	// because runIncomingResume released the pair startIncomingLive held; if the
	// release regressed, the range stays exhausted and ReservePair returns
	// ErrNoFreePort.
	if _, _, err := m.migPorts.ReservePair(); err != nil {
		t.Errorf("ports not released after resume: %v", err)
	}
}

// TestRunIncomingResume_FallbackToBootExportOnEmptyRecord proves the defensive
// fallback: a migration record carrying no ExportIDs (unexpected, e.g. a legacy
// or partially-written record) still tears down the boot export ["exp0"] rather
// than leaking it. Drives runIncomingResume directly with the empty-ExportIDs
// record so the fallback branch is the only thing under test.
func TestRunIncomingResume_FallbackToBootExportOnEmptyRecord(t *testing.T) {
	m := newTestManager(t)
	m.migDialQMPTarget = func(socket string) (qemu.LiveTargetConn, error) { return stubTargetConn{}, nil }
	var gotSpec qemu.LiveTargetSpec
	m.migRunLiveTarget = func(ctx context.Context, conn qemu.LiveTargetConn, spec qemu.LiveTargetSpec) error {
		gotSpec = spec
		return nil
	}

	v := m.seedRunningVM(t, "demo")
	migID := uuid.New()
	ram, nbd, err := m.migPorts.ReservePair()
	if err != nil {
		t.Fatalf("ReservePair: %v", err)
	}
	// Record with an empty ExportIDs slice: the fallback must kick in.
	m.migrations.Put(&migration.Record{
		MigrationID: migID, VMID: v.ID, VMName: v.Name,
		Role: migration.RoleTarget, Mode: migration.ModeLive, Phase: migration.PhaseSetup,
		Port: ram, NBDPort: nbd, ExportIDs: nil, CreatedAt: time.Now().UTC(),
	})

	task := m.tasks.Create(TaskKindVMMigrate, v.ID)
	m.runIncomingResume(context.Background(), task.ID, migID, v.ID, ram, nbd)

	if diff := cmp.Diff([]string{"exp0"}, gotSpec.ExportIDs); diff != "" {
		t.Errorf("fallback LiveTargetSpec.ExportIDs mismatch (-want +got):\n%s", diff)
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
		MemoryMib:      512,
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

// newTestManagerWithFabric mirrors newTestManager but retains the spy fabric so
// a test can assert which host taps were created/attached. The resume seams are
// stubbed to fast no-ops (as newTestManager does) so the detached
// runIncomingResume goroutine drives to StatusRunning without a real QMP dial.
func newTestManagerWithFabric(t *testing.T) (*Manager, *netfabric.FakeFabric) {
	t.Helper()
	cfg, poolRoot, poolName := newTestConfig(t)
	fab := &netfabric.FakeFabric{}
	m, err := New(cfg, fab, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.AddPool(poolName, poolRoot); err != nil {
		t.Fatalf("AddPool(%s): %v", poolName, err)
	}
	m.migWaitNBDReady = func(context.Context, string) error { return nil }
	m.migDialQMPTarget = func(string) (qemu.LiveTargetConn, error) { return stubTargetConn{}, nil }
	m.migRunLiveTarget = func(context.Context, qemu.LiveTargetConn, qemu.LiveTargetSpec) error { return nil }
	t.Cleanup(m.resumeWG.Wait)
	return m, fab
}

// TestStartIncomingLiveMaterializesNICs is the teeth for target NIC setup: the target must
// create the migrated VM's NIC taps (attached to the overlay bridge) BEFORE
// launching the incoming qemu, so the resumed guest has network. Before the fix
// AdoptForMigration never set v.NICs and startIncomingLive never materialized
// taps, so the fabric saw zero CreateTap calls.
func TestStartIncomingLiveMaterializesNICs(t *testing.T) {
	m, fab := newTestManagerWithFabric(t)
	m.migLaunchIncoming = func(context.Context, *VM, qemu.LiveIncomingSpec) error { return nil }
	m.migDialQMP = func(string) (qemu.LiveSourceConn, error) { return &fakeLiveConn{}, nil }
	m.migCreateDisk = func(context.Context, string, int64) error { return nil }

	spec := IncomingSpec{
		MigrationID:    uuid.New(),
		VMUUID:         uuid.New(),
		VMName:         "demo",
		VCPUs:          1,
		MemoryMib:      512,
		PoolName:       m.defaultTestPool(),
		Architecture:   "amd64",
		Mode:           "live",
		ExpectedSize:   1 << 30,
		DiskSizeBytes:  1 << 30,
		SourceIdentity: "CN=node-src",
		BindHost:       "10.0.0.2",
		NICs: []netfabric.NIC{{
			ID: uuid.New(), Bridge: "otvb100", MAC: "52:54:00:00:00:01",
			Model: "virtio", MTU: 1390, DeviceOrder: 0,
			IPv4: netip.MustParseAddr("10.42.0.5"),
		}},
	}

	if _, err := m.startIncomingLive(context.Background(), spec); err != nil {
		t.Fatalf("startIncomingLive: %v", err)
	}

	if got := len(fab.CreateTapCalls); got != 1 {
		t.Errorf("created taps = %d, want 1 (migrated NIC not materialized)", got)
	}
	if len(fab.AttachTapCalls) != 1 || fab.AttachTapCalls[0].Bridge != "otvb100" {
		t.Errorf("attached bridges = %v, want one attach to otvb100", fab.AttachTapCalls)
	}
}

// TestRunIncomingResumeSendsGARP is the teeth for target GARP emission: after the target
// resumes the live-migrated guest (StatusRunning), the agent emits one
// gratuitous ARP per NIC with an IPv4 on the NIC's bridge, to refresh neighbor
// ARP caches. The resume runs in the detached goroutine startIncomingLive
// spawns; resumeWG.Wait (registered by newTestManagerWithFabric) awaits it.
func TestRunIncomingResumeSendsGARP(t *testing.T) {
	m, fab := newTestManagerWithFabric(t)
	m.migLaunchIncoming = func(context.Context, *VM, qemu.LiveIncomingSpec) error { return nil }
	m.migDialQMP = func(string) (qemu.LiveSourceConn, error) { return &fakeLiveConn{}, nil }
	m.migCreateDisk = func(context.Context, string, int64) error { return nil }

	vmUUID := uuid.New()
	spec := IncomingSpec{
		MigrationID:    uuid.New(),
		VMUUID:         vmUUID,
		VMName:         "garpvm",
		VCPUs:          1,
		MemoryMib:      512,
		PoolName:       m.defaultTestPool(),
		Architecture:   "amd64",
		Mode:           "live",
		ExpectedSize:   1 << 30,
		DiskSizeBytes:  1 << 30,
		SourceIdentity: "CN=node-src",
		BindHost:       "10.0.0.2",
		NICs: []netfabric.NIC{{
			ID: uuid.New(), Bridge: "otvb100", MAC: "52:54:00:00:00:01",
			Model: "virtio", MTU: 1390, DeviceOrder: 0,
			IPv4: netip.MustParseAddr("10.42.0.5"),
		}},
	}

	if _, err := m.startIncomingLive(context.Background(), spec); err != nil {
		t.Fatalf("startIncomingLive: %v", err)
	}
	// Await the detached resume reaching StatusRunning (and, via resumeWG at
	// cleanup, fully finishing) so the post-resume GARP has been issued.
	waitStatus(t, m, vmUUID, StatusRunning)
	m.resumeWG.Wait()

	calls := fab.SendGARPCalls
	if len(calls) != 1 {
		t.Fatalf("GARPs sent = %d, want 1", len(calls))
	}
	got := calls[0]
	if got.Bridge != "otvb100" || got.MAC != "52:54:00:00:00:01" || got.IP != netip.MustParseAddr("10.42.0.5") {
		t.Errorf("SendGARP call = %+v, want bridge=otvb100 mac=52:54:00:00:00:01 ip=10.42.0.5", got)
	}
}

// announceRecorderConn is a stubTargetConn that records announce-self calls so a
// test can assert the post-resume QMP announce-self fired on the live conn.
type announceRecorderConn struct {
	stubTargetConn
	announced int
	params    qemu.AnnounceParameters
}

func (c *announceRecorderConn) AnnounceSelf(p qemu.AnnounceParameters) error {
	c.announced++
	c.params = p
	return nil
}

// TestRunIncomingResumeAnnouncesSelf is the teeth for target announce-self: after the
// target resumes the live-migrated guest (StatusRunning), the agent issues one
// QMP announce-self on the same live conn so a learning bridge / switch relearns
// the guest's MAC on this node's port. Drives the real resume path (the detached
// goroutine startIncomingLive spawns) with migDialQMPTarget overridden to return
// the recording conn; resumeWG.Wait awaits the resume finishing.
func TestRunIncomingResumeAnnouncesSelf(t *testing.T) {
	m, _ := newTestManagerWithFabric(t)
	m.migLaunchIncoming = func(context.Context, *VM, qemu.LiveIncomingSpec) error { return nil }
	m.migDialQMP = func(string) (qemu.LiveSourceConn, error) { return &fakeLiveConn{}, nil }
	m.migCreateDisk = func(context.Context, string, int64) error { return nil }

	rec := &announceRecorderConn{}
	m.migDialQMPTarget = func(string) (qemu.LiveTargetConn, error) { return rec, nil }

	vmUUID := uuid.New()
	spec := IncomingSpec{
		MigrationID:    uuid.New(),
		VMUUID:         vmUUID,
		VMName:         "announcevm",
		VCPUs:          1,
		MemoryMib:      512,
		PoolName:       m.defaultTestPool(),
		Architecture:   "amd64",
		Mode:           "live",
		ExpectedSize:   1 << 30,
		DiskSizeBytes:  1 << 30,
		SourceIdentity: "CN=node-src",
		BindHost:       "10.0.0.2",
		NICs: []netfabric.NIC{{
			ID: uuid.New(), Bridge: "otvb100", MAC: "52:54:00:00:00:01",
			Model: "virtio", MTU: 1390, DeviceOrder: 0,
			IPv4: netip.MustParseAddr("10.42.0.5"),
		}},
	}

	if _, err := m.startIncomingLive(context.Background(), spec); err != nil {
		t.Fatalf("startIncomingLive: %v", err)
	}
	waitStatus(t, m, vmUUID, StatusRunning)
	m.resumeWG.Wait()

	if rec.announced != 1 {
		t.Errorf("announce-self calls = %d, want 1", rec.announced)
	}
	if rec.params.Rounds == 0 {
		t.Errorf("announce params not passed: %+v", rec.params)
	}
}

// TestTeardownDepartedSource_RemovesVMAndDisk is the unit test for the helper:
// after a successful outgoing LIVE migration the source VM is garbage (the guest
// is now on the target), so teardownDepartedSource must drop the in-memory record
// and remove the per-VM disk dir.
func TestTeardownDepartedSource_RemovesVMAndDisk(t *testing.T) {
	m := newTestManager(t)
	vmID := uuid.New()
	v, err := m.AdoptForMigration(AdoptSpec{
		UUID: vmID, Name: "ex", VCPUs: 1, MemoryMib: 512,
		PoolName: m.defaultTestPool(), Architecture: qemu.ArchAMD64,
	})
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	diskDir := filepath.Dir(v.DiskPath)
	if err := os.MkdirAll(diskDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(diskDir, "disk.qcow2"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	m.teardownDepartedSource(v)
	if _, err := m.Get(vmID); err == nil {
		t.Errorf("source vm still present after teardownDepartedSource")
	}
	if _, err := os.Stat(diskDir); !os.IsNotExist(err) {
		t.Errorf("source disk dir still present: %v", err)
	}
}

// TestTeardownDepartedSource_ClosesMux is the seam test for the logs/console
// hang after a live migration. When the source VM departs, its serial
// multiplexer must be Close'd so every attached logs/console subscriber sees
// Done(). That Done is exactly what makes the source agent's /logs streamLogs
// loop return -> the HTTP body EOFs -> the CP-side follow relay observes the
// upstream break and reattaches to the target. Without detachMux the mux is
// leaked open (pump goroutine + log file handle never released) and the stream
// hangs forever: the operator's `vm logs -f` freezes at cutover until a manual
// reconnect. The teeth: with the detachMux call removed, sub.Done() never fires
// and this test times out.
func TestTeardownDepartedSource_ClosesMux(t *testing.T) {
	m := newTestManager(t)

	// A fake qemu serial socket so attachMux can dial a real multiplexer. The
	// short /tmp path avoids the ~104-char sun_path limit on the state dir.
	sockDir, err := os.MkdirTemp("/tmp", "oxsock")
	if err != nil {
		t.Skipf("cannot create short socket dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sockPath := filepath.Join(sockDir, "c.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(conn net.Conn) { _, _ = io.Copy(io.Discard, conn) }(c)
		}
	}()

	vmID := uuid.New()
	v, err := m.AdoptForMigration(AdoptSpec{
		UUID: vmID, Name: "ex", VCPUs: 1, MemoryMib: 512,
		PoolName: m.defaultTestPool(), Architecture: qemu.ArchAMD64,
	})
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	v.ConsoleSocket = sockPath
	if err := m.attachMux(discardLogger(), v); err != nil {
		t.Fatalf("attachMux: %v", err)
	}
	mux := m.GetMux(vmID)
	if mux == nil {
		t.Fatal("GetMux(ex) = nil after attachMux")
	}
	sub := mux.SubscribeLogs(0, true) // follow, no history

	m.teardownDepartedSource(v)

	select {
	case <-sub.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber Done() did not fire: teardownDepartedSource left the mux open (logs/console hang after migration)")
	}
	if m.GetMux(vmID) != nil {
		t.Error("GetMux(ex) != nil after teardownDepartedSource: mux not detached")
	}
}

// TestRunOutgoingLive_SuccessTearsDownSource is the seam test: it drives the REAL
// runOutgoingLive success path (migRunLiveSource stubbed to nil) and asserts the
// source VM was torn down BEFORE the task is marked success - so "migration
// completed" implies "source released" and a reverse migration adopts cleanly
// instead of racing the CP's slow async DeleteVMOnSource. Mirrors the stubbing in
// TestRunOutgoingLive_NoPoweroff_DrivesToCompleted (seedRunningVM + migRunLiveSource
// + migDialQMP -> fakeLiveConn).
func TestRunOutgoingLive_SuccessTearsDownSource(t *testing.T) {
	m := newTestManager(t)
	v := m.seedRunningVM(t, "demo")
	diskDir := filepath.Dir(v.DiskPath)

	m.migRunLiveSource = func(ctx context.Context, conn qemu.LiveSourceConn, spec qemu.LiveSourceSpec, report func(qemu.LiveProgress)) error {
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

	// The source VM must be gone from the manager (torn down on success).
	if _, err := m.Get(v.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("after outgoing-live success, Get(%s) = %v, want ErrNotFound (source VM must be torn down)", v.ID, err)
	}
	// And its per-VM disk dir removed.
	if _, err := os.Stat(diskDir); !os.IsNotExist(err) {
		t.Errorf("source disk dir still present after outgoing-live success: %v", err)
	}

	// The tracking task must be success and the record completed.
	tk := m.tasks.Get(task.ID)
	if tk == nil || tk.Status != TaskStatusSuccess {
		t.Errorf("task = %v, want status success", tk)
	}
	rec, ok := m.Migrations().Get(migID)
	if !ok || rec.Phase != migration.PhaseCompleted {
		t.Errorf("record = %+v, want phase completed", rec)
	}
}

// TestRunOutgoingLive_GuardConfirmsCompletedBeforeTeardown is the happy-path
// teeth for the convergence guard: migRunLiveSource returns nil AND the direct
// query-migrate reports "completed", so the guard passes and the departed source
// is torn down (VM gone, disk dir removed), the task reaches success. This proves
// the common case is unaffected by the guard. (The unit-fake killQEMU is a quiet
// no-op on the missing pidfile, so source teardown is observed via the VM record
// being removed + the disk dir gone, exactly as the existing teardown tests do.)
func TestRunOutgoingLive_GuardConfirmsCompletedBeforeTeardown(t *testing.T) {
	m := newTestManager(t)
	v := m.seedRunningVM(t, "demo")
	diskDir := filepath.Dir(v.DiskPath)

	m.migRunLiveSource = func(ctx context.Context, conn qemu.LiveSourceConn, spec qemu.LiveSourceSpec, report func(qemu.LiveProgress)) error {
		return nil
	}
	// Explicit "completed" status from the post-migration query-migrate.
	m.migDialQMP = func(socket string) (qemu.LiveSourceConn, error) {
		return &fakeLiveConn{migrateStatus: "completed"}, nil
	}

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

	// Guard passed: the departed source was torn down (VM gone, disk removed).
	if _, err := m.Get(v.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("after confirmed completion, Get(%s) = %v, want ErrNotFound (source torn down)", v.ID, err)
	}
	if _, err := os.Stat(diskDir); !os.IsNotExist(err) {
		t.Errorf("source disk dir still present after confirmed completion: %v (want torn down)", err)
	}

	tk := m.tasks.Get(task.ID)
	if tk == nil || tk.Status != TaskStatusSuccess {
		t.Errorf("task = %v, want status success", tk)
	}
}

// TestRunOutgoingLive_GuardKeepsSourceOnFalseCompletion is the never-lose-the-VM
// teeth: migRunLiveSource returns nil (claims success) BUT the direct
// query-migrate reports a non-completed status. The guard must REFUSE to tear
// down the source - the source VM record + disk must survive - and the task must
// be finalized FAILED (fail-safe-to-source). Revert-to-confirm: removing the
// guard from runOutgoingLive makes this test see the source torn down (VM gone,
// disk removed) and the task success.
func TestRunOutgoingLive_GuardKeepsSourceOnFalseCompletion(t *testing.T) {
	m := newTestManager(t)
	v := m.seedRunningVM(t, "demo")
	diskDir := filepath.Dir(v.DiskPath)

	// RunLiveSource claims success (nil) - the completion-detection bug case.
	m.migRunLiveSource = func(ctx context.Context, conn qemu.LiveSourceConn, spec qemu.LiveSourceSpec, report func(qemu.LiveProgress)) error {
		return nil
	}
	// But the independent query-migrate disagrees: status is "active", not
	// "completed". The guard must keep the source.
	m.migDialQMP = func(socket string) (qemu.LiveSourceConn, error) {
		return &fakeLiveConn{migrateStatus: "active"}, nil
	}

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

	waitPhase(t, m, migID, "failed")

	// The source VM must SURVIVE - this is the never-destroy-the-only-copy
	// invariant. It must still be present in the manager and its disk dir intact.
	if _, err := m.Get(v.ID); err != nil {
		t.Errorf("source VM removed on false completion: Get(%s) = %v, want it kept (the only copy must not be destroyed)", v.ID, err)
	}
	if _, err := os.Stat(diskDir); err != nil {
		t.Errorf("source disk dir removed on false completion: %v (must be kept)", err)
	}

	// The task must be finalized FAILED.
	tk := m.tasks.Get(task.ID)
	if tk == nil || tk.Status != TaskStatusFailed {
		t.Errorf("task = %v, want status failed (fail-safe-to-source)", tk)
	}
}

// cancelInWindowConn fires a cancel from inside AnnounceSelf, which
// runIncomingResume calls AFTER it flips the guest to StatusRunning but (in the
// pre-fix ordering) BEFORE it stamps the record terminal. This deterministically
// lands a CancelMigration inside the post-cont window the P1b fix closes.
type cancelInWindowConn struct {
	stubTargetConn
	cancel func()
}

func (c *cancelInWindowConn) AnnounceSelf(qemu.AnnounceParameters) error {
	c.cancel()
	return nil
}

// TestRunIncomingResume_PostContCancelNoOps is the teeth for P1b: a cancel that
// arrives AFTER the target guest resumed (cont done, VM StatusRunning) must
// no-op, never reap the now-live guest. It drives the REAL runIncomingResume and
// the REAL CancelMigration entry; the cancel is fired from inside the post-resume
// AnnounceSelf step, landing in the exact window. Pre-fix the record is not yet
// terminal at that point, so cancelLive takes the target reap arm
// (teardownIncomingTarget -> killQEMU + removeAdoptedVM) and destroys the live
// guest: the VM disappears (Get returns ErrNotFound) and its disk dir is removed.
// Post-fix the record is already PhaseCompleted, so cancelLive no-ops.
func TestRunIncomingResume_PostContCancelNoOps(t *testing.T) {
	m := newTestManager(t)

	v := m.seedRunningVM(t, "demo")
	diskDir := filepath.Dir(v.DiskPath)

	migID := uuid.New()
	ram, nbd, err := m.migPorts.ReservePair()
	if err != nil {
		t.Fatalf("ReservePair: %v", err)
	}
	m.migrations.Put(&migration.Record{
		MigrationID: migID, VMID: v.ID, VMName: v.Name,
		Role: migration.RoleTarget, Mode: migration.ModeLive, Phase: migration.PhaseSetup,
		Port: ram, NBDPort: nbd, ExportIDs: []string{"exp0"}, CreatedAt: time.Now().UTC(),
	})

	conn := &cancelInWindowConn{cancel: func() { m.CancelMigration(migID) }}
	m.migDialQMPTarget = func(string) (qemu.LiveTargetConn, error) { return conn, nil }
	m.migRunLiveTarget = func(context.Context, qemu.LiveTargetConn, qemu.LiveTargetSpec) error {
		return nil
	}

	task := m.tasks.Create(TaskKindVMMigrate, v.ID)
	m.runIncomingResume(context.Background(), task.ID, migID, v.ID, ram, nbd)

	// The just-resumed guest must survive the in-window cancel: still present,
	// still running, disk intact. Read the raw in-map record (m.Get probes the
	// absent pidfile and would report stopped under the unit fake).
	m.mu.Lock()
	got, present := m.vms[v.ID]
	var gotStatus Status
	if present {
		gotStatus = got.Status
	}
	m.mu.Unlock()
	if !present {
		t.Fatalf("VM %s removed by in-window cancel; want the resumed guest kept", v.ID)
	}
	if gotStatus != StatusRunning {
		t.Errorf("status = %v, want running (cancel must not stop a resumed guest)", gotStatus)
	}
	if _, err := os.Stat(diskDir); err != nil {
		t.Errorf("disk dir removed by in-window cancel: %v (removeAdoptedVM must not run post-cont)", err)
	}

	// The migration reports completed, not cancelled: the cancel no-ops because
	// the record was stamped terminal at cont success.
	view, ok := m.GetMigration(migID)
	if !ok {
		t.Fatalf("GetMigration(%s) ok=false", migID)
	}
	if view.Phase != string(migration.PhaseCompleted) {
		t.Errorf("phase = %q, want completed (post-cont cancel must no-op)", view.Phase)
	}
}

// TestCancelLive_RunningGuestRefusesReap is the teeth for the residual
// stamp-move TOCTOU (guard (b), "never reap a running guest"). CancelMigration
// snapshots the record and passes that STALE snapshot to cancelLive; if the
// snapshot was read a hair before the resume goroutine stamps PhaseCompleted,
// the snapshot's Terminal() is still false and the RoleTarget arm would proceed
// to teardownIncomingTarget -> killQEMU + removeAdoptedVM on a guest that just
// went LIVE (irreversible: the dest disk is the only copy post-cutover).
//
// We simulate the race directly: a target VM already at StatusRunning plus a
// NON-terminal record. cancelLive must refuse the reap on the in-memory
// StatusRunning signal and no-op: the VM stays present, its disk dir intact, and
// the record is NOT cancelled.
func TestCancelLive_RunningGuestRefusesReap(t *testing.T) {
	m := newTestManager(t)

	v := m.seedRunningVM(t, "demo")
	diskDir := filepath.Dir(v.DiskPath)

	ram, nbd, err := m.migPorts.ReservePair()
	if err != nil {
		t.Fatalf("ReservePair: %v", err)
	}
	migID := uuid.New()
	// A non-terminal record simulates the stale snapshot CancelMigration read a
	// hair before the resume goroutine stamped PhaseCompleted.
	stale := migration.Record{
		MigrationID: migID, VMID: v.ID, VMName: v.Name,
		Role: migration.RoleTarget, Mode: migration.ModeLive, Phase: migration.PhaseActive,
		Port: ram, NBDPort: nbd, CreatedAt: time.Now().UTC(),
	}
	m.migrations.Put(&stale)

	view, ok := m.cancelLive(migID, stale)
	if !ok {
		t.Fatalf("cancelLive returned ok=false")
	}

	// The running guest must survive: still present, still running, disk intact.
	m.mu.Lock()
	got, present := m.vms[v.ID]
	var gotStatus Status
	if present {
		gotStatus = got.Status
	}
	m.mu.Unlock()
	if !present {
		t.Fatalf("VM %s removed by cancel of a running guest; want it kept", v.ID)
	}
	if gotStatus != StatusRunning {
		t.Errorf("status = %v, want running (cancel must not reap a running guest)", gotStatus)
	}
	if _, err := os.Stat(diskDir); err != nil {
		t.Errorf("disk dir removed by cancel of a running guest: %v (removeAdoptedVM must not run)", err)
	}

	// The record must NOT be flipped to cancelled: the guard refuses the reap and
	// leaves the record untouched.
	if view.Phase == string(migration.PhaseCancelled) {
		t.Errorf("phase = %q, want not-cancelled (running guest must not be cancel-reaped)", view.Phase)
	}
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
