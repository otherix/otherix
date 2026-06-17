// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// copyConvert is a test stand-in for qemu.ConvertTo: it copies src to dst
// verbatim (no format change), exercising produceBlob's hash/rename/sidecar
// tail without shelling out to qemu-img.
func copyConvert(_ context.Context, src, dst string) error {
	b, err := os.ReadFile(src) //nolint:gosec // test-local path
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}

func TestProduceBlob_HashesAndAtomicRenamesWithSidecar(t *testing.T) {
	dir := t.TempDir()
	snapshotsDir := filepath.Join(dir, "snapshots")

	src := filepath.Join(dir, "src-disk0.qcow2")
	body := qcow2Body(0x42)
	if err := os.WriteFile(src, body, 0o600); err != nil {
		t.Fatalf("write src: %v", err)
	}
	wantSHA := shaHex(body)

	res, err := produceBlob(context.Background(), src, snapshotsDir, copyConvert)
	if err != nil {
		t.Fatalf("produceBlob: %v", err)
	}
	if res.SHA256 != wantSHA {
		t.Errorf("SHA256 = %q, want %q", res.SHA256, wantSHA)
	}
	wantPath := filepath.Join(snapshotsDir, wantSHA+".qcow2")
	if res.Path != wantPath {
		t.Errorf("Path = %q, want %q", res.Path, wantPath)
	}
	if res.SizeBytes != int64(len(body)) {
		t.Errorf("SizeBytes = %d, want %d", res.SizeBytes, len(body))
	}

	// Blob lands at the content-addressed path.
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("blob not at %q: %v", wantPath, err)
	}
	// Sidecar matches the digest.
	sidecar, err := os.ReadFile(wantPath + ".sha256")
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if string(sidecar) != wantSHA {
		t.Errorf("sidecar = %q, want %q", sidecar, wantSHA)
	}
	// No staging leftovers.
	staging := filepath.Join(snapshotsDir, ".staging")
	if entries, err := os.ReadDir(staging); err == nil && len(entries) != 0 {
		t.Errorf(".staging has leftovers: %v", entries)
	}

	// Idempotent re-run: same content -> same digest, no error, blob still there.
	res2, err := produceBlob(context.Background(), src, snapshotsDir, copyConvert)
	if err != nil {
		t.Fatalf("produceBlob re-run: %v", err)
	}
	if res2.SHA256 != wantSHA || res2.Path != wantPath {
		t.Errorf("re-run mismatch: %+v, want sha %q path %q", res2, wantSHA, wantPath)
	}
	if res2.SizeBytes != int64(len(body)) {
		t.Errorf("re-run SizeBytes = %d, want %d", res2.SizeBytes, len(body))
	}
}

func TestProduceBlob_RejectsNonQcow2(t *testing.T) {
	dir := t.TempDir()
	snapshotsDir := filepath.Join(dir, "snapshots")
	src := filepath.Join(dir, "bad.qcow2")
	if err := os.WriteFile(src, []byte("not-a-qcow2-header"), 0o600); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if _, err := produceBlob(context.Background(), src, snapshotsDir, copyConvert); err == nil {
		t.Fatal("produceBlob on non-qcow2 = nil error, want failure")
	}
}

func TestListSnapshots_PairsBlobsWithSidecars(t *testing.T) {
	dir := t.TempDir()
	snapshotsDir := filepath.Join(dir, "snapshots")

	// Empty / absent dir lists nothing.
	got, err := ListSnapshots(snapshotsDir)
	if err != nil {
		t.Fatalf("ListSnapshots empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListSnapshots empty = %v, want 0", got)
	}

	// Produce two distinct blobs.
	for _, tag := range []byte{0x01, 0x02} {
		src := filepath.Join(dir, "src.qcow2")
		if err := os.WriteFile(src, qcow2Body(tag), 0o600); err != nil {
			t.Fatalf("write src: %v", err)
		}
		if _, err := produceBlob(context.Background(), src, snapshotsDir, copyConvert); err != nil {
			t.Fatalf("produceBlob: %v", err)
		}
	}
	// A blob without a sidecar is skipped.
	if err := os.WriteFile(filepath.Join(snapshotsDir, "orphan.qcow2"), qcow2Body(0x09), 0o600); err != nil {
		t.Fatalf("write orphan: %v", err)
	}

	got, err = ListSnapshots(snapshotsDir)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListSnapshots = %d entries, want 2: %+v", len(got), got)
	}
	for _, b := range got {
		if !isHexSHA256Lower(b.SHA256) {
			t.Errorf("blob SHA256 = %q, not a hex digest", b.SHA256)
		}
		if b.SizeBytes != int64(len(qcow2Body(0x01))) {
			t.Errorf("blob SizeBytes = %d, want %d", b.SizeBytes, len(qcow2Body(0x01)))
		}
	}
}
