// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/netfabric"
	"github.com/otherix/otherix/internal/agent/state"
)

// LOAD-BEARING INVARIANT (read before touching this package's tests):
// withProbeIncoming and withKillSpy mutate the PACKAGE-LEVEL seams
// probeRecoveredIncoming / killRecoveredIncoming, which New copies into the
// Manager during construction. This is parallel-safe ONLY because no test in
// internal/agent/vm/ calls t.Parallel(): any parallel test reaching New would
// race the swap and restore, which previously produced a -race CI flake. Do NOT
// add t.Parallel() to any test in this package without first converting these
// seams to per-Manager injection (e.g. functional options passed to New).

// withProbeIncoming swaps the package-level probe hook for the duration of a
// test and restores it. The replay loop consults this hook inside New, so a
// recovery test must script it BEFORE constructing the Manager. Mutates a
// package-level seam - see the LOAD-BEARING INVARIANT note above (no
// t.Parallel() in this package).
func withProbeIncoming(t *testing.T, fn func(qmpSocket, pidFile string) incomingProbe) {
	t.Helper()
	prev := probeRecoveredIncoming
	probeRecoveredIncoming = fn
	t.Cleanup(func() { probeRecoveredIncoming = prev })
}

// seedIncomingMeta writes a meta.json for a VM in StatusMigratingIncoming with a
// stub disk on disk under the given pool root, returning the VM id and disk dir.
// Mirrors how AdoptForMigration lays out a live target before replay.
func seedIncomingMeta(t *testing.T, stateDir, poolRoot string, nics []state.NICMeta) (uuid.UUID, string) {
	t.Helper()
	id := uuid.New()
	disk := filepath.Join(poolRoot, "vms", id.String(), "disk.qcow2")
	if err := os.MkdirAll(filepath.Dir(disk), 0o750); err != nil {
		t.Fatalf("mkdir disk parent: %v", err)
	}
	if err := os.WriteFile(disk, []byte("qcow2-stub"), 0o600); err != nil {
		t.Fatalf("write stub disk: %v", err)
	}
	vmDir := filepath.Join(stateDir, id.String())
	now := time.Now().UTC()
	if err := state.WriteMeta(vmDir, &state.VMMeta{
		VMID:          id,
		Name:          "incoming-" + id.String()[:8],
		VCPUs:         1,
		MemoryMB:      512,
		PoolName:      "default",
		Architecture:  "amd64",
		DiskPath:      disk,
		QMPSocket:     filepath.Join(vmDir, "qmp.sock"),
		ConsoleSocket: filepath.Join(vmDir, "console.sock"),
		PIDFile:       filepath.Join(vmDir, "qemu.pid"),
		Status:        string(StatusMigratingIncoming),
		CreatedAt:     now,
		UpdatedAt:     now,
		NICs:          nics,
		Migrated:      true,
	}); err != nil {
		t.Fatalf("WriteMeta: %v", err)
	}
	return id, filepath.Dir(disk)
}

// withKillSpy swaps the package-level killRecoveredIncoming hook for a recording
// spy and restores it. The replay loop binds m.killIncoming to this hook inside
// New, so a recovery test must install the spy BEFORE constructing the Manager.
// Returns a func reporting the recorded kill ids. Mutates a package-level seam -
// see the LOAD-BEARING INVARIANT note above (no t.Parallel() in this package).
func withKillSpy(t *testing.T) func() []uuid.UUID {
	t.Helper()
	var mu sync.Mutex
	var killed []uuid.UUID
	prev := killRecoveredIncoming
	killRecoveredIncoming = func(_ *Manager, v *VM) {
		mu.Lock()
		killed = append(killed, v.ID)
		mu.Unlock()
	}
	t.Cleanup(func() { killRecoveredIncoming = prev })
	return func() []uuid.UUID {
		mu.Lock()
		defer mu.Unlock()
		return append([]uuid.UUID(nil), killed...)
	}
}

// rawStatus reads the in-memory recorded status without an observedStatus probe.
func rawStatus(t *testing.T, m *Manager, id uuid.UUID) (Status, bool) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vms[id]
	if !ok {
		return "", false
	}
	return v.Status, true
}

// TestRecoverIncoming_Paused reaps a genuine orphaned paused incoming: the VM
// goes StatusFailed, killIncoming runs, teardownNICs runs (fabric DeleteTap),
// removeAdoptedVM does NOT run (disk dir kept), and the VM stays in the map.
func TestRecoverIncoming_Paused(t *testing.T) {
	cfg, poolRoot, _ := newTestConfig(t)
	withProbeIncoming(t, func(string, string) incomingProbe {
		return incomingProbe{alive: true, queried: true, runState: "paused"}
	})

	nicID := uuid.New()
	nics := []state.NICMeta{{
		ID: nicID, Bridge: "br0", MAC: "52:54:00:00:00:01",
		Model: "virtio-net-pci", MTU: 1500, DeviceOrder: 0,
		TapName: netfabric.NIC{ID: nicID}.TapName(),
	}}
	wantTap := netfabric.NIC{ID: nicID}.TapName()
	id, diskDir := seedIncomingMeta(t, cfg.StatePath, poolRoot, nics)

	kills := withKillSpy(t)
	fabric := &netfabric.FakeFabric{}

	m, err := New(cfg, fabric, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if got, ok := rawStatus(t, m, id); !ok || got != StatusFailed {
		t.Errorf("status = %v (present=%v), want failed", got, ok)
	}
	gotKills := kills()
	if len(gotKills) != 1 || gotKills[0] != id {
		t.Errorf("killIncoming calls = %v, want exactly [%s]", gotKills, id)
	}
	if len(fabric.DeleteTapCalls) != 1 || fabric.DeleteTapCalls[0] != wantTap {
		t.Errorf("DeleteTap calls = %v, want [%s]", fabric.DeleteTapCalls, wantTap)
	}
	if _, err := os.Stat(diskDir); err != nil {
		t.Errorf("disk dir removed on recovery: %v (removeAdoptedVM must NOT run)", err)
	}
}

// TestRecoverIncoming_Running promotes a post-cutover resumed guest whose status
// write was lost: the VM goes StatusRunning, killIncoming is NOT called, and the
// meta is persisted StatusRunning.
func TestRecoverIncoming_Running(t *testing.T) {
	cfg, poolRoot, _ := newTestConfig(t)
	withProbeIncoming(t, func(string, string) incomingProbe {
		return incomingProbe{alive: true, queried: true, runState: "running"}
	})

	id, diskDir := seedIncomingMeta(t, cfg.StatePath, poolRoot, nil)

	kills := withKillSpy(t)

	m, err := New(cfg, &netfabric.FakeFabric{}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if got, ok := rawStatus(t, m, id); !ok || got != StatusRunning {
		t.Errorf("status = %v (present=%v), want running (promote)", got, ok)
	}
	if n := len(kills()); n != 0 {
		t.Errorf("killIncoming called %d times, want 0 (never kill a running guest)", n)
	}
	if _, err := os.Stat(diskDir); err != nil {
		t.Errorf("disk dir removed: %v (must never delete on recovery)", err)
	}

	// Meta persisted StatusRunning so a subsequent restart does not re-enter the
	// incoming-recovery branch.
	meta, err := state.ReadMeta(filepath.Join(cfg.StatePath, id.String()))
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if meta.Status != string(StatusRunning) {
		t.Errorf("persisted status = %q, want running", meta.Status)
	}
}

// TestRecoverIncoming_Dead marks a process-gone incoming StatusFailed without
// killing, tears down its taps, and keeps the disk dir.
func TestRecoverIncoming_Dead(t *testing.T) {
	cfg, poolRoot, _ := newTestConfig(t)
	withProbeIncoming(t, func(string, string) incomingProbe {
		return incomingProbe{alive: false}
	})

	nicID := uuid.New()
	nics := []state.NICMeta{{
		ID: nicID, Bridge: "br0", MAC: "52:54:00:00:00:02",
		Model: "virtio-net-pci", MTU: 1500, DeviceOrder: 0,
		TapName: netfabric.NIC{ID: nicID}.TapName(),
	}}
	wantTap := netfabric.NIC{ID: nicID}.TapName()
	id, diskDir := seedIncomingMeta(t, cfg.StatePath, poolRoot, nics)

	kills := withKillSpy(t)

	fabric := &netfabric.FakeFabric{}
	m, err := New(cfg, fabric, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if got, ok := rawStatus(t, m, id); !ok || got != StatusFailed {
		t.Errorf("status = %v (present=%v), want failed", got, ok)
	}
	if n := len(kills()); n != 0 {
		t.Errorf("killIncoming called %d times, want 0 (no qemu to kill)", n)
	}
	if len(fabric.DeleteTapCalls) != 1 || fabric.DeleteTapCalls[0] != wantTap {
		t.Errorf("DeleteTap calls = %v, want [%s]", fabric.DeleteTapCalls, wantTap)
	}
	if _, err := os.Stat(diskDir); err != nil {
		t.Errorf("disk dir removed on recovery: %v", err)
	}
}

// TestRecoverIncoming_Inconclusive marks an alive-but-undialable incoming
// StatusFailed WITHOUT killing and keeps the disk (fail toward inaction).
func TestRecoverIncoming_Inconclusive(t *testing.T) {
	cfg, poolRoot, _ := newTestConfig(t)
	withProbeIncoming(t, func(string, string) incomingProbe {
		// Alive, but query-status failed (queried=false) - inconclusive.
		return incomingProbe{alive: true, queried: false}
	})

	id, diskDir := seedIncomingMeta(t, cfg.StatePath, poolRoot, nil)

	kills := withKillSpy(t)

	m, err := New(cfg, &netfabric.FakeFabric{}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if got, ok := rawStatus(t, m, id); !ok || got != StatusFailed {
		t.Errorf("status = %v (present=%v), want failed", got, ok)
	}
	if n := len(kills()); n != 0 {
		t.Errorf("killIncoming called %d times, want 0 (inconclusive must not kill)", n)
	}
	if _, err := os.Stat(diskDir); err != nil {
		t.Errorf("disk dir removed on inconclusive recovery: %v", err)
	}
}

// TestObservedStatus_Incoming covers the observedStatus arm: a dead pidfile
// reports StatusFailed; a live pid reports StatusMigratingIncoming.
func TestObservedStatus_Incoming(t *testing.T) {
	m := newTestManager(t)

	deadVM := &VM{
		ID:      uuid.New(),
		Name:    "dead-incoming",
		Status:  StatusMigratingIncoming,
		PIDFile: filepath.Join(t.TempDir(), "absent.pid"),
	}
	if got := m.observedStatus(deadVM); got != StatusFailed {
		t.Errorf("observedStatus(dead incoming) = %v, want failed", got)
	}

	pidDir := t.TempDir()
	pidFile := filepath.Join(pidDir, "live.pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatalf("write pidfile: %v", err)
	}
	liveVM := &VM{
		ID:      uuid.New(),
		Name:    "live-incoming",
		Status:  StatusMigratingIncoming,
		PIDFile: pidFile,
	}
	if got := m.observedStatus(liveVM); got != StatusMigratingIncoming {
		t.Errorf("observedStatus(live incoming) = %v, want migrating_incoming", got)
	}
}
