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
	// Stray UUID directory without meta.json — must be skipped with warning.
	if err := os.Mkdir(filepath.Join(stateDir, uuid.NewString()), 0o755); err != nil {
		t.Fatalf("mkdir orphan: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	metas, err := ScanState(stateDir, log)
	if err != nil {
		t.Fatalf("ScanState: %v", err)
	}

	if len(metas) != 2 {
		t.Fatalf("got %d metas, want 2", len(metas))
	}
	seen := map[string]bool{}
	for _, m := range metas {
		seen[m.VMID.String()] = true
	}
	if !seen[id1] || !seen[id2] {
		t.Errorf("missing expected ids; seen=%v", seen)
	}
}

func TestScanState_CreatesMissingDir(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	stateDir := filepath.Join(t.TempDir(), "fresh")
	metas, err := ScanState(stateDir, log)
	if err != nil {
		t.Fatalf("ScanState(fresh): %v", err)
	}
	if len(metas) != 0 {
		t.Errorf("len = %d, want 0", len(metas))
	}
	if _, err := os.Stat(stateDir); err != nil {
		t.Errorf("ScanState should mkdir state dir: %v", err)
	}
}
