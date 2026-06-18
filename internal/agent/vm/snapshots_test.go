// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"unicode"
)

// sameFileIno asserts that two FileInfos refer to the same on-disk file
// (same inode), proving a blob was reused in place rather than re-renamed
// over. os.SameFile compares device + inode, which is exactly the "did the
// file get replaced" signal we need.
func sameFileIno(a, b os.FileInfo) bool {
	return os.SameFile(a, b)
}

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

	// Idempotency teeth: capture the blob's identity (inode, via FileInfo)
	// BEFORE the re-run so we can prove the 2nd call REUSED the existing blob
	// rather than re-staging and renaming a fresh file over it. A digest/path
	// match alone is too weak - re-writing identical content yields the same
	// digest/path/size, so it stays green even if the reuse short-circuit is
	// removed. Inode identity is the signal that catches that regression.
	preInfo, err := os.Stat(wantPath)
	if err != nil {
		t.Fatalf("stat blob before re-run: %v", err)
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

	// The blob on disk must be the SAME file (same inode) as before the
	// re-run: reuse means we never rename a fresh staging file over it.
	postInfo, err := os.Stat(wantPath)
	if err != nil {
		t.Fatalf("stat blob after re-run: %v", err)
	}
	if !sameFileIno(preInfo, postInfo) {
		t.Errorf("re-run replaced the blob (inode changed); want reuse of the existing file")
	}
	// Reuse must also leave no staging file behind: the staging copy is
	// dropped (by the deferred remove) rather than renamed into place.
	if entries, err := os.ReadDir(staging); err == nil && len(entries) != 0 {
		t.Errorf(".staging has leftovers after reuse re-run: %v", entries)
	}
}

// TestProduceBlob_SidecarNeverPrecedesBlob pins the durability ordering
// invariant: a {sha}.qcow2.sha256 sidecar must NEVER exist on disk pointing at
// a missing blob - the sidecar is written strictly AFTER the blob lands via
// the atomic rename. We force the rename to fail (its target path is occupied
// by a directory) and then assert that no sidecar was left behind. With the
// production order (rename then sidecar) the rename fails first, so the sidecar
// is never written. If a regression moved the sidecar write BEFORE the rename,
// the sidecar would already be on disk when the rename fails, and this test
// catches it.
func TestProduceBlob_SidecarNeverPrecedesBlob(t *testing.T) {
	dir := t.TempDir()
	snapshotsDir := filepath.Join(dir, "snapshots")
	if err := os.MkdirAll(snapshotsDir, 0o750); err != nil {
		t.Fatalf("mkdir snapshots: %v", err)
	}

	src := filepath.Join(dir, "src-disk0.qcow2")
	body := qcow2Body(0x42)
	if err := os.WriteFile(src, body, 0o600); err != nil {
		t.Fatalf("write src: %v", err)
	}
	wantSHA := shaHex(body)

	// Occupy the content-addressed blob path with a directory so the atomic
	// rename of the staging file fails. readCachedImage treats a directory as
	// "not present", so the reuse short-circuit is skipped and produceBlob
	// proceeds to the rename. (A non-empty directory cannot be the rename
	// target, so os.Rename returns an error.)
	blobPath := filepath.Join(snapshotsDir, wantSHA+".qcow2")
	if err := os.MkdirAll(filepath.Join(blobPath, "occupied"), 0o750); err != nil {
		t.Fatalf("occupy blob path with dir: %v", err)
	}
	sidecarPath := blobPath + ".sha256"

	if _, err := produceBlob(context.Background(), src, snapshotsDir, copyConvert); err == nil {
		t.Fatal("produceBlob with occupied rename target = nil error, want failure")
	}

	// The invariant: no sidecar may exist pointing at the (still missing) blob.
	if _, err := os.Stat(sidecarPath); !os.IsNotExist(err) {
		t.Errorf("sidecar exists at %q after rename failure (err=%v); a sidecar must never precede its blob", sidecarPath, err)
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

// TestQemuBackupIDs_WithinNodeNameLimit pins the QEMU block-node-name constraint
// that the real-agent smoke caught: blockdev-add rejects a node-name longer than
// 31 chars ("Node name too long"), and a full uuid suffix (36 chars) overflowed
// it. Both the job-id and the node-name must stay within the limit and start
// with a letter (a valid qemu identifier).
func TestQemuBackupIDs_WithinNodeNameLimit(t *testing.T) {
	const qemuNodeNameMax = 31
	for i := 0; i < 100; i++ {
		jobID, nodeName := qemuBackupIDs()
		for _, id := range []struct{ kind, val string }{{"jobID", jobID}, {"nodeName", nodeName}} {
			if len(id.val) > qemuNodeNameMax {
				t.Errorf("%s = %q, len %d > qemu limit %d", id.kind, id.val, len(id.val), qemuNodeNameMax)
			}
			if id.val == "" || !unicode.IsLetter(rune(id.val[0])) {
				t.Errorf("%s = %q must start with a letter", id.kind, id.val)
			}
		}
		if jobID == nodeName {
			t.Errorf("jobID and nodeName must differ, both %q", jobID)
		}
	}
}
