// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/agent/artifactstore"
	"github.com/otherix/otherix/internal/agent/blobpeer"
)

// tierTokenVerifier authorises exactly one (token, digest) pair and serves that
// digest's bytes - a minimal blobpeer.TokenVerifier for the holder httptest.
type tierTokenVerifier struct {
	token  string
	digest string
}

func (v tierTokenVerifier) Verify(token, digest string) (string, bool) {
	if token != v.token || digest != v.digest {
		return "", false
	}
	return digest, true
}

// TestPullBlobTierImageWritesImageStore drives PullBlob end to end with
// tier="image": a holder httptest serves the blob from its artifact store; the
// consumer Manager has BOTH stores; the pulled blob must land in the IMAGE store
// and NOT the artifact store.
func TestPullBlobTierImageWritesImageStore(t *testing.T) {
	content := []byte("pinned-image-bytes")
	sum := sha256.Sum256(content)
	d := hex.EncodeToString(sum[:])
	const token = "otx_tier_img"

	// Holder: serves the blob from its own store over a TLS httptest server.
	holderStore, err := artifactstore.NewForTesting(t.TempDir())
	if err != nil {
		t.Fatalf("holder store: %v", err)
	}
	if err := holderStore.Put(d, bytes.NewReader(content)); err != nil {
		t.Fatalf("seed holder: %v", err)
	}
	holder := httptest.NewTLSServer(blobpeer.NewServeHandler(holderStore, tierTokenVerifier{token: token, digest: d}))
	defer holder.Close()

	// Consumer Manager with both tiers.
	art, err := artifactstore.NewForTesting(t.TempDir())
	if err != nil {
		t.Fatalf("art store: %v", err)
	}
	img, err := artifactstore.NewForTesting(t.TempDir())
	if err != nil {
		t.Fatalf("img store: %v", err)
	}
	m := &Manager{
		tasks:         NewTaskStore(),
		log:           discardLogger(),
		artifactStore: art,
		imageStore:    img,
	}

	task, err := m.PullBlob(context.Background(), holder.Client(), d, token, holder.URL, "", 0, "image")
	if err != nil {
		t.Fatalf("PullBlob: %v", err)
	}

	if got := waitForTaskTerminal(t, m, task.ID, 5*time.Second); got.Status != TaskStatusSuccess {
		t.Fatalf("pull task status = %v, want success (error=%+v)", got.Status, got.Error)
	}

	if !img.Has(d) {
		t.Errorf("image store missing blob after tier=image pull")
	}
	if art.Has(d) {
		t.Errorf("artifact store has blob after tier=image pull (should be image-only)")
	}
}

// TestPullBlobDefaultTierWritesArtifactStore pins the default (unchanged) path:
// an empty tier lands the blob in the artifact store, not the image store.
func TestPullBlobDefaultTierWritesArtifactStore(t *testing.T) {
	content := []byte("artifact-default-bytes")
	sum := sha256.Sum256(content)
	d := hex.EncodeToString(sum[:])
	const token = "otx_tier_art"

	holderStore, err := artifactstore.NewForTesting(t.TempDir())
	if err != nil {
		t.Fatalf("holder store: %v", err)
	}
	if err := holderStore.Put(d, bytes.NewReader(content)); err != nil {
		t.Fatalf("seed holder: %v", err)
	}
	holder := httptest.NewTLSServer(blobpeer.NewServeHandler(holderStore, tierTokenVerifier{token: token, digest: d}))
	defer holder.Close()

	art, err := artifactstore.NewForTesting(t.TempDir())
	if err != nil {
		t.Fatalf("art store: %v", err)
	}
	img, err := artifactstore.NewForTesting(t.TempDir())
	if err != nil {
		t.Fatalf("img store: %v", err)
	}
	m := &Manager{
		tasks:         NewTaskStore(),
		log:           discardLogger(),
		artifactStore: art,
		imageStore:    img,
	}

	task, err := m.PullBlob(context.Background(), holder.Client(), d, token, holder.URL, "", 0, "")
	if err != nil {
		t.Fatalf("PullBlob: %v", err)
	}

	if got := waitForTaskTerminal(t, m, task.ID, 5*time.Second); got.Status != TaskStatusSuccess {
		t.Fatalf("pull task status = %v, want success (error=%+v)", got.Status, got.Error)
	}

	if !art.Has(d) {
		t.Errorf("artifact store missing blob after default-tier pull")
	}
	if img.Has(d) {
		t.Errorf("image store has blob after default-tier pull (should be artifact-only)")
	}
}
