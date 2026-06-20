// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package artifactstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
	"time"
)

// scrubTestStore builds a Store under a temp dir within the allowlist prefix is
// not possible (the prefix is /var/lib/otherix/); use newForTest which skips the
// prefix check. Confirm newForTest exists (store.go New delegates to it).
func scrubTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := newForTest(t.TempDir())
	if err != nil {
		t.Fatalf("newForTest: %v", err)
	}
	return s
}

func putBlob(t *testing.T, s *Store, content []byte) string {
	t.Helper()
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	if err := s.Put(digest, bytes.NewReader(content)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	return digest
}

// backdateSidecar makes a blob eligible for scrubbing by aging its sidecar mtime
// past any MinReverifyInterval the test uses.
func backdateSidecar(t *testing.T, s *Store, digest string) {
	t.Helper()
	old := time.Now().Add(-300 * time.Hour)
	if err := os.Chtimes(s.sidecarPath(digest), old, old); err != nil {
		t.Fatalf("backdate sidecar: %v", err)
	}
}

func TestScrubHealthyBlobVerifiedNotDeleted(t *testing.T) {
	s := scrubTestStore(t)
	d := putBlob(t, s, []byte("healthy content"))
	backdateSidecar(t, s, d)

	res, err := s.Scrub(context.Background(), ScrubOptions{
		MinReverifyInterval: time.Hour, MaxBytesPerPass: 1 << 30, Now: time.Now,
	})
	if err != nil {
		t.Fatalf("Scrub: %v", err)
	}
	if res.Verified != 1 || res.CorruptDeleted != 0 {
		t.Errorf("res = %+v, want Verified=1 CorruptDeleted=0", res)
	}
	if !s.Has(d) {
		t.Errorf("healthy blob was deleted")
	}
	// sidecar mtime bumped to ~now (no longer eligible)
	fi, _ := os.Stat(s.sidecarPath(d))
	if time.Since(fi.ModTime()) > time.Minute {
		t.Errorf("sidecar mtime not bumped on verify: %v", fi.ModTime())
	}
}

func TestScrubCorruptBlobDeleted(t *testing.T) {
	s := scrubTestStore(t)
	d := putBlob(t, s, []byte("original content for digest"))
	backdateSidecar(t, s, d)
	// Tamper: overwrite the blob bytes in place, leave the sidecar.
	if err := os.WriteFile(s.blobPath(d), []byte("CORRUPTED DIFFERENT BYTES"), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	res, err := s.Scrub(context.Background(), ScrubOptions{
		MinReverifyInterval: time.Hour, MaxBytesPerPass: 1 << 30, Now: time.Now,
	})
	if err != nil {
		t.Fatalf("Scrub: %v", err)
	}
	if res.CorruptDeleted != 1 || res.Verified != 0 {
		t.Errorf("res = %+v, want CorruptDeleted=1 Verified=0", res)
	}
	if s.Has(d) {
		t.Errorf("corrupt blob was NOT deleted")
	}
	if _, err := os.Stat(s.sidecarPath(d)); !os.IsNotExist(err) {
		t.Errorf("corrupt blob sidecar was not removed")
	}
}

func TestScrubUnreadableBlobSkippedNotDeleted(t *testing.T) {
	s := scrubTestStore(t)
	d := putBlob(t, s, []byte("content"))
	backdateSidecar(t, s, d)
	// Make the blob unreadable so blobHashesTo returns an I/O error (not a mismatch).
	if err := os.Chmod(s.blobPath(d), 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer func() { _ = os.Chmod(s.blobPath(d), 0o644) }()

	res, err := s.Scrub(context.Background(), ScrubOptions{
		MinReverifyInterval: time.Hour, MaxBytesPerPass: 1 << 30, Now: time.Now,
	})
	if err != nil {
		t.Fatalf("Scrub: %v", err)
	}
	if res.CorruptDeleted != 0 {
		t.Errorf("an unreadable blob must be skipped, not deleted; res = %+v", res)
	}
	_ = os.Chmod(s.blobPath(d), 0o644)
	if !s.Has(d) {
		t.Errorf("unreadable blob was deleted on an I/O error (must fail toward inaction)")
	}
}

func TestScrubSkipsRecentlyVerified(t *testing.T) {
	s := scrubTestStore(t)
	d := putBlob(t, s, []byte("fresh")) // sidecar mtime ~now
	// Do NOT backdate: it is younger than MinReverifyInterval.
	res, err := s.Scrub(context.Background(), ScrubOptions{
		MinReverifyInterval: time.Hour, MaxBytesPerPass: 1 << 30, Now: time.Now,
	})
	if err != nil {
		t.Fatalf("Scrub: %v", err)
	}
	if res.Verified != 0 {
		t.Errorf("a recently-verified blob must be skipped; res = %+v", res)
	}
	_ = d
}

func TestScrubBudgetStopsAfterCeiling(t *testing.T) {
	s := scrubTestStore(t)
	// Two ~100-byte blobs, budget 50 -> only the first (coldest) is processed.
	d1 := putBlob(t, s, bytes.Repeat([]byte("a"), 100))
	d2 := putBlob(t, s, bytes.Repeat([]byte("b"), 100))
	// Age d1 older than d2 so d1 sorts first (coldest).
	older := time.Now().Add(-300 * time.Hour)
	newer := time.Now().Add(-200 * time.Hour)
	_ = os.Chtimes(s.sidecarPath(d1), older, older)
	_ = os.Chtimes(s.sidecarPath(d2), newer, newer)

	res, err := s.Scrub(context.Background(), ScrubOptions{
		MinReverifyInterval: time.Hour, MaxBytesPerPass: 50, Now: time.Now,
	})
	if err != nil {
		t.Fatalf("Scrub: %v", err)
	}
	if res.Verified != 1 {
		t.Errorf("budget 50 over two 100-byte blobs should verify exactly 1, got %+v", res)
	}
}

func TestScrubTryLockContentionSkips(t *testing.T) {
	s := scrubTestStore(t)
	d := putBlob(t, s, []byte("original"))
	backdateSidecar(t, s, d)
	if err := os.WriteFile(s.blobPath(d), []byte("CORRUPT"), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	// tryLock always reports contention -> the victim is never re-hashed or deleted.
	res, err := s.Scrub(context.Background(), ScrubOptions{
		MinReverifyInterval: time.Hour, MaxBytesPerPass: 1 << 30, Now: time.Now,
		TryLock: func(string) (func(), bool) { return nil, false },
	})
	if err != nil {
		t.Fatalf("Scrub: %v", err)
	}
	if res.CorruptDeleted != 0 || !s.Has(d) {
		t.Errorf("a locked (being-cloned) blob must be skipped, not deleted; res=%+v has=%v", res, s.Has(d))
	}
}
