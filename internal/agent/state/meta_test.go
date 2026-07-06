// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package state

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func sampleMeta() *VMMeta {
	return &VMMeta{
		VMID:          uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"),
		Name:          "test-vm",
		VCPUs:         4,
		MemoryMB:      4096,
		PoolName:      "default",
		Architecture:  "arm64",
		DiskPath:      "/var/lib/otherix/pools/default/vms/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/disk.qcow2",
		QMPSocket:     "/var/lib/otherix/vms/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/qmp.sock",
		ConsoleSocket: "/var/lib/otherix/vms/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/console.sock",
		PIDFile:       "/var/lib/otherix/vms/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/qemu.pid",
		Status:        "running",
		CreatedAt:     time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC),
		UpdatedAt:     time.Date(2026, 5, 9, 12, 5, 0, 0, time.UTC),
	}
}

func TestWriteMeta_ReadMeta_RoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vm")
	want := sampleMeta()

	if err := WriteMeta(dir, want); err != nil {
		t.Fatalf("WriteMeta: %v", err)
	}

	got, err := ReadMeta(dir)
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}

	if got.VMID != want.VMID || got.Name != want.Name || got.Status != want.Status {
		t.Errorf("read mismatch: got %+v, want %+v", got, want)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("CreatedAt: got %v, want %v", got.CreatedAt, want.CreatedAt)
	}
}

func TestVMMetaMigratedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := &VMMeta{
		VMID:     uuid.New(),
		Name:     "demo",
		PoolName: "default",
		DiskPath: "/d/disk.qcow2",
		Status:   "stopped",
		Migrated: true,
	}
	if err := WriteMeta(dir, in); err != nil {
		t.Fatalf("WriteMeta() error = %v", err)
	}
	out, err := ReadMeta(dir)
	if err != nil {
		t.Fatalf("ReadMeta() error = %v", err)
	}
	if !out.Migrated {
		t.Errorf("ReadMeta().Migrated = false, want true")
	}
}

func TestWriteMeta_AtomicTempCleared(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vm")
	if err := WriteMeta(dir, sampleMeta()); err != nil {
		t.Fatalf("WriteMeta: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, MetaFileName+".tmp")); !os.IsNotExist(err) {
		t.Errorf("temp file should not survive write, stat=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, MetaFileName)); err != nil {
		t.Errorf("final meta.json missing: %v", err)
	}
}

func TestReadMeta_Missing(t *testing.T) {
	_, err := ReadMeta(t.TempDir())
	if !os.IsNotExist(err) {
		t.Errorf("ReadMeta(empty dir) error = %v, want os.ErrNotExist", err)
	}
}

func TestScanState_PicksUpVMDirs(t *testing.T) {
	stateDir := t.TempDir()
	id1 := uuid.New().String()
	id2 := uuid.New().String()

	for _, id := range []string{id1, id2} {
		m := sampleMeta()
		m.VMID = uuid.MustParse(id)
		if err := WriteMeta(filepath.Join(stateDir, id), m); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	// Stray non-UUID directory — must be ignored.
	if err := os.Mkdir(filepath.Join(stateDir, "scratch"), 0o755); err != nil {
		t.Fatalf("mkdir scratch: %v", err)
	}
	// Stray UUID directory without meta.json — must be skipped with warning
	// AND counted (it may be a live VM whose meta was lost).
	if err := os.Mkdir(filepath.Join(stateDir, uuid.NewString()), 0o755); err != nil {
		t.Fatalf("mkdir orphan: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	metas, skipped, err := ScanState(stateDir, log)
	if err != nil {
		t.Fatalf("ScanState: %v", err)
	}

	if len(metas) != 2 {
		t.Fatalf("got %d metas, want 2", len(metas))
	}
	// The one UUID dir without meta.json is a skip; the non-UUID "scratch" dir
	// is NOT (it was never a VM). skipped>0 disables the destructive orphan-tap
	// sweep upstream.
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1 (the UUID dir without meta.json)", skipped)
	}
	seen := map[string]bool{}
	for _, m := range metas {
		seen[m.VMID.String()] = true
	}
	if !seen[id1] || !seen[id2] {
		t.Errorf("missing expected ids; seen=%v", seen)
	}
}

func TestScanState_CountsCorruptMetaAsSkipped(t *testing.T) {
	stateDir := t.TempDir()

	// A valid VM.
	good := uuid.New().String()
	m := sampleMeta()
	m.VMID = uuid.MustParse(good)
	if err := WriteMeta(filepath.Join(stateDir, good), m); err != nil {
		t.Fatalf("seed good: %v", err)
	}

	// A UUID dir whose meta.json exists but is unparseable garbage: this models
	// a live VM whose meta was corrupted. It must be skipped AND counted, so the
	// orphan-tap sweep upstream fails closed rather than severing that VM's taps.
	bad := uuid.New().String()
	badDir := filepath.Join(stateDir, bad)
	if err := os.MkdirAll(badDir, 0o750); err != nil {
		t.Fatalf("mkdir bad: %v", err)
	}
	if err := os.WriteFile(filepath.Join(badDir, MetaFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt meta: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	metas, skipped, err := ScanState(stateDir, log)
	if err != nil {
		t.Fatalf("ScanState: %v", err)
	}
	if len(metas) != 1 {
		t.Errorf("len(metas) = %d, want 1 (the good VM)", len(metas))
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1 (the corrupt meta)", skipped)
	}
}

func TestScanState_CreatesMissingDir(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	stateDir := filepath.Join(t.TempDir(), "fresh")
	metas, skipped, err := ScanState(stateDir, log)
	if err != nil {
		t.Fatalf("ScanState(fresh): %v", err)
	}
	if len(metas) != 0 {
		t.Errorf("len = %d, want 0", len(metas))
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0 (fresh dir)", skipped)
	}
	if _, err := os.Stat(stateDir); err != nil {
		t.Errorf("ScanState should mkdir state dir: %v", err)
	}
}
