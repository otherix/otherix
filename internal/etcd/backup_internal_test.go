// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPruneSnapshots(t *testing.T) {
	dir := t.TempDir()
	snaps := []string{
		"otherix-20260101T000001Z.db",
		"otherix-20260101T000002Z.db",
		"otherix-20260101T000003Z.db",
		"otherix-20260101T000004Z.db",
	}
	// Files that look adjacent but must never be pruned (wrong prefix/suffix).
	others := []string{"otherix-notes.txt", "random.db", "keep-me"}
	for _, n := range append(append([]string{}, snaps...), others...) {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", n, err)
		}
	}

	removed, err := pruneSnapshots(dir, 2)
	if err != nil {
		t.Fatalf("pruneSnapshots: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}

	for _, n := range []string{"otherix-20260101T000003Z.db", "otherix-20260101T000004Z.db"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("newest snapshot %s should remain: %v", n, err)
		}
	}
	for _, n := range []string{"otherix-20260101T000001Z.db", "otherix-20260101T000002Z.db"} {
		if _, err := os.Stat(filepath.Join(dir, n)); !os.IsNotExist(err) {
			t.Errorf("oldest snapshot %s should be pruned", n)
		}
	}
	for _, n := range others {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("non-snapshot file %s must not be pruned: %v", n, err)
		}
	}
}

func TestPruneSnapshotsNoopCases(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"otherix-20260101T000001Z.db", "otherix-20260101T000002Z.db"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// retention >= count keeps everything.
	if removed, err := pruneSnapshots(dir, 5); err != nil || removed != 0 {
		t.Errorf("pruneSnapshots(retention>count) = (%d, %v), want (0, nil)", removed, err)
	}
	// retention <= 0 disables pruning.
	if removed, err := pruneSnapshots(dir, 0); err != nil || removed != 0 {
		t.Errorf("pruneSnapshots(0) = (%d, %v), want (0, nil)", removed, err)
	}
}
