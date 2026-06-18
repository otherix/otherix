// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/otherix/otherix/internal/agent/artifactstore"
	"github.com/otherix/otherix/internal/agent/heartbeat"
)

// sha256Hex returns the lowercase hex sha256 of b.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestServeTokenStore_VerifyScopesToPrimedDigest pins the C1 token gate: a
// primed token authorizes serving exactly its digest and nothing else, an
// unprimed token is rejected, and a dropped token (serve expiry) no longer
// verifies. The per-listener cluster-CA client-cert gate is enforced separately
// in server.go's tls.Config; this covers the token half.
func TestServeTokenStore_VerifyScopesToPrimedDigest(t *testing.T) {
	digestA := strings.Repeat("a", 64)
	digestB := strings.Repeat("b", 64)
	s := newServeTokenStore()
	s.prime("tok-1", digestA)

	if got, ok := s.Verify("tok-1", digestA); !ok || got != digestA {
		t.Errorf("Verify(tok-1, A) = (%q, %v), want (%q, true)", got, ok, digestA)
	}
	if _, ok := s.Verify("tok-1", digestB); ok {
		t.Errorf("Verify(tok-1, B) = ok, want token may not serve a different digest")
	}
	if _, ok := s.Verify("unprimed", digestA); ok {
		t.Errorf("Verify(unprimed, A) = ok, want unprimed token rejected")
	}

	s.drop("tok-1")
	if _, ok := s.Verify("tok-1", digestA); ok {
		t.Errorf("Verify(tok-1, A) after drop = ok, want dropped token rejected")
	}
}

// TestBlobInventoryAdapter_NodeBlobs confirms the heartbeat BlobLister adapter
// maps the artifact store's blob entries to heartbeat.BlobReport and reports
// ok=true (including the empty inventory case).
func TestBlobInventoryAdapter_NodeBlobs(t *testing.T) {
	store, err := artifactstore.NewForTesting(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatalf("NewForTesting: %v", err)
	}
	a := blobInventoryAdapter{store: store, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	reports, ok := a.NodeBlobs()
	if !ok {
		t.Fatalf("NodeBlobs ok = false on empty store, want true")
	}
	if len(reports) != 0 {
		t.Errorf("NodeBlobs len = %d, want 0 on empty store", len(reports))
	}

	digest := mustPutBlob(t, store, []byte("hello blob"))
	reports, ok = a.NodeBlobs()
	if !ok {
		t.Fatalf("NodeBlobs ok = false, want true")
	}
	want := heartbeat.BlobReport{Digest: digest, SizeBytes: int64(len("hello blob"))}
	if len(reports) != 1 || reports[0] != want {
		t.Errorf("NodeBlobs = %+v, want [%+v]", reports, want)
	}
}

// mustPutBlob writes content into store under its sha256 digest and returns the
// digest.
func mustPutBlob(t *testing.T, store *artifactstore.Store, content []byte) string {
	t.Helper()
	digest := sha256Hex(content)
	if err := store.Put(digest, strings.NewReader(string(content))); err != nil {
		t.Fatalf("store.Put: %v", err)
	}
	return digest
}
