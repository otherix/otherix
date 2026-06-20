// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/agent/artifactstore"
	"github.com/otherix/otherix/internal/config"
)

func scrubStore(t *testing.T) *artifactstore.Store {
	t.Helper()
	s, err := artifactstore.NewForTest(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	return s
}

func putAndAge(t *testing.T, s *artifactstore.Store, content []byte) string {
	t.Helper()
	sum := sha256.Sum256(content)
	d := hex.EncodeToString(sum[:])
	if err := s.Put(d, bytes.NewReader(content)); err != nil {
		t.Fatalf("put: %v", err)
	}
	old := time.Now().Add(-300 * time.Hour)
	_ = os.Chtimes(s.SidecarPathForTest(d), old, old)
	return d
}

func TestBlobScrubberSweepsBothStores(t *testing.T) {
	art := scrubStore(t)
	img := scrubStore(t)
	dArt := putAndAge(t, art, []byte("artifact original"))
	dImg := putAndAge(t, img, []byte("image original"))
	// tamper both
	_ = os.WriteFile(art.BlobPathForTest(dArt), []byte("ART CORRUPT"), 0o644)
	_ = os.WriteFile(img.BlobPathForTest(dImg), []byte("IMG CORRUPT"), 0o644)

	sc := &blobScrubber{
		artifactStore: art,
		imageStore:    img,
		imageTryLock:  nil, // no contention in this test
		cfg:           config.ScrubConfig{Interval: time.Hour, MinReverifyInterval: time.Hour, MaxBytesPerPass: 1 << 30},
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	sc.sweep(context.Background())

	if art.Has(dArt) {
		t.Errorf("corrupt artifact blob not scrubbed")
	}
	if img.Has(dImg) {
		t.Errorf("corrupt image blob not scrubbed")
	}
}

func TestBlobScrubberDisabledNoop(t *testing.T) {
	art := scrubStore(t)
	d := putAndAge(t, art, []byte("x"))
	_ = os.WriteFile(art.BlobPathForTest(d), []byte("corrupt"), 0o644)
	sc := &blobScrubber{
		artifactStore: art,
		cfg:           config.ScrubConfig{}, // all zero = disabled
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	sc.sweep(context.Background())
	if !art.Has(d) {
		t.Errorf("disabled scrubber must not delete anything")
	}
}
