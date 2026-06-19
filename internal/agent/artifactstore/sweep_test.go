// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package artifactstore

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func putValidSweep(t *testing.T, s *Store, content []byte) string {
	t.Helper()
	digest := digestOf(content)
	if err := s.Put(digest, bytes.NewReader(content)); err != nil {
		t.Fatalf("Put = %v", err)
	}
	return digest
}

func TestSweepStagingRemovesStaleSparesFresh(t *testing.T) {
	s := newStore(t)
	stale := filepath.Join(s.stagingDir(), "stale.tmp")
	fresh := filepath.Join(s.stagingDir(), "fresh.tmp")
	if err := os.WriteFile(stale, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fresh, []byte("y"), 0o640); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	removed, err := s.SweepStaging(time.Hour)
	if err != nil {
		t.Fatalf("SweepStaging = %v", err)
	}
	if removed != 1 {
		t.Errorf("SweepStaging removed = %d, want 1", removed)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale staging file still present")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh staging file removed, want spared: %v", err)
	}
}

func TestSweepStagingZeroAgeRemovesAll(t *testing.T) {
	s := newStore(t)
	f := filepath.Join(s.stagingDir(), "live.tmp")
	if err := os.WriteFile(f, []byte("z"), 0o640); err != nil {
		t.Fatal(err)
	}
	removed, err := s.SweepStaging(0)
	if err != nil {
		t.Fatalf("SweepStaging(0) = %v", err)
	}
	if removed != 1 {
		t.Errorf("SweepStaging(0) removed = %d, want 1", removed)
	}
}

func TestSweepSidecarlessRepairsValidBlob(t *testing.T) {
	s := newStore(t)
	digest := putValidSweep(t, s, []byte("hello artifact"))
	if err := os.Remove(s.sidecarPath(digest)); err != nil {
		t.Fatal(err)
	}
	repaired, deleted, err := s.SweepSidecarless()
	if err != nil {
		t.Fatalf("SweepSidecarless = %v", err)
	}
	if repaired != 1 || deleted != 0 {
		t.Errorf("SweepSidecarless = (%d, %d), want (1, 0)", repaired, deleted)
	}
	if !s.Has(digest) {
		t.Errorf("valid blob was deleted, want repaired")
	}
	if _, err := os.Stat(s.sidecarPath(digest)); err != nil {
		t.Errorf("sidecar not rewritten: %v", err)
	}
}

func TestSweepSidecarlessDeletesCorruptBlob(t *testing.T) {
	s := newStore(t)
	corrupt := "0000000000000000000000000000000000000000000000000000000000000000"
	if err := os.WriteFile(s.blobPath(corrupt), []byte("not the right content"), 0o640); err != nil {
		t.Fatal(err)
	}
	repaired, deleted, err := s.SweepSidecarless()
	if err != nil {
		t.Fatalf("SweepSidecarless = %v", err)
	}
	if repaired != 0 || deleted != 1 {
		t.Errorf("SweepSidecarless = (%d, %d), want (0, 1)", repaired, deleted)
	}
	if _, err := os.Stat(s.blobPath(corrupt)); !os.IsNotExist(err) {
		t.Errorf("corrupt blob still present")
	}
}

func TestSweepSidecarlessDeletesOrphanSidecar(t *testing.T) {
	s := newStore(t)
	orphan := "1111111111111111111111111111111111111111111111111111111111111111"
	if err := os.WriteFile(s.sidecarPath(orphan), []byte(orphan), 0o644); err != nil {
		t.Fatal(err)
	}
	_, deleted, err := s.SweepSidecarless()
	if err != nil {
		t.Fatalf("SweepSidecarless = %v", err)
	}
	if deleted != 1 {
		t.Errorf("SweepSidecarless deleted = %d, want 1 (orphan sidecar)", deleted)
	}
	if _, err := os.Stat(s.sidecarPath(orphan)); !os.IsNotExist(err) {
		t.Errorf("orphan sidecar still present")
	}
}

func TestSweepSidecarlessLeavesHealthyBlob(t *testing.T) {
	s := newStore(t)
	digest := putValidSweep(t, s, []byte("healthy"))
	repaired, deleted, err := s.SweepSidecarless()
	if err != nil {
		t.Fatalf("SweepSidecarless = %v", err)
	}
	if repaired != 0 || deleted != 0 {
		t.Errorf("SweepSidecarless healthy = (%d, %d), want (0, 0)", repaired, deleted)
	}
	if !s.Has(digest) {
		t.Errorf("healthy blob removed")
	}
}
