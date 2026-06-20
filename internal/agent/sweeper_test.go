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

	"github.com/otherix/otherix/internal/agent/artifactstore"
)

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
