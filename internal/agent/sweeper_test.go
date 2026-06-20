// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agent

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/agent/artifactstore"
)

// stageOrphan writes an interrupted-Put staging temp into store and returns its
// path.
func stageOrphan(t *testing.T, store *artifactstore.Store) string {
	t.Helper()
	staging := filepath.Join(store.Root(), "blobs", ".staging")
	if err := os.MkdirAll(staging, 0o750); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(staging, "interrupted.tmp")
	if err := os.WriteFile(orphan, []byte("partial"), 0o640); err != nil {
		t.Fatal(err)
	}
	return orphan
}

// TestArtifactSweeperRunSkipsBootPass asserts Run performs the periodic pass only:
// a fresh staging temp survives a tick window because Run no longer clears all
// staging at startup (that is BootSweep's job, run synchronously before serving).
func TestArtifactSweeperRunSkipsBootPass(t *testing.T) {
	artStore, err := artifactstore.NewForTesting(t.TempDir())
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	imgStore, err := artifactstore.NewForTesting(t.TempDir())
	if err != nil {
		t.Fatalf("image store: %v", err)
	}
	orphan := stageOrphan(t, artStore)

	sw := newArtifactSweeper(artStore, imgStore, sweeperDiscardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Run returns at once; no boot pass means no immediate sweep.
	_ = sw.Run(ctx)

	// Give any (erroneous) immediate sweep a chance to have fired.
	time.Sleep(20 * time.Millisecond)
	if _, err := os.Stat(orphan); err != nil {
		t.Errorf("Run removed fresh staging temp; boot pass should not run in Run: %v", err)
	}
}

// TestArtifactSweeperBootSweepClearsBothStores asserts BootSweep synchronously
// clears all staging on both the artifact and image stores.
func TestArtifactSweeperBootSweepClearsBothStores(t *testing.T) {
	artStore, err := artifactstore.NewForTesting(t.TempDir())
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	imgStore, err := artifactstore.NewForTesting(t.TempDir())
	if err != nil {
		t.Fatalf("image store: %v", err)
	}
	artOrphan := stageOrphan(t, artStore)
	imgOrphan := stageOrphan(t, imgStore)

	sw := newArtifactSweeper(artStore, imgStore, sweeperDiscardLogger())
	sw.BootSweep(context.Background())

	if _, err := os.Stat(artOrphan); !os.IsNotExist(err) {
		t.Errorf("artifact-store staging orphan not removed by BootSweep")
	}
	if _, err := os.Stat(imgOrphan); !os.IsNotExist(err) {
		t.Errorf("image-store staging orphan not removed by BootSweep")
	}
}

func sweeperDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestArtifactSweeperSweepsImageStore(t *testing.T) {
	artStore, err := artifactstore.NewForTesting(t.TempDir())
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	imgStore, err := artifactstore.NewForTesting(t.TempDir())
	if err != nil {
		t.Fatalf("image store: %v", err)
	}

	// An interrupted-Put staging orphan in the IMAGE store.
	staging := filepath.Join(imgStore.Root(), "blobs", ".staging")
	if err := os.MkdirAll(staging, 0o750); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(staging, "interrupted.tmp")
	if err := os.WriteFile(orphan, []byte("partial"), 0o640); err != nil {
		t.Fatal(err)
	}

	// A valid but sidecarless blob in the IMAGE store (a crashed sidecar write).
	content := []byte("image blob bytes")
	digest := sha256Hex(content)
	if err := imgStore.Put(digest, bytes.NewReader(content)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := os.Remove(imgStore.SidecarPathForTest(digest)); err != nil {
		t.Fatalf("remove sidecar: %v", err)
	}

	sw := newArtifactSweeper(artStore, imgStore, sweeperDiscardLogger())
	sw.sweep(context.Background(), 0)

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("image-store staging orphan not removed")
	}
	if !imgStore.Has(digest) {
		t.Errorf("valid sidecarless image blob was deleted, want repaired+kept")
	}
	if _, err := os.Stat(imgStore.SidecarPathForTest(digest)); err != nil {
		t.Errorf("image-store sidecar not repaired: %v", err)
	}
}
