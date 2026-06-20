// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package blobpeer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/otherix/otherix/internal/agent/artifactstore"
)

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// staticTokenVerifierStub is a TokenVerifier test double: it authorizes exactly
// one (token, digest) pair and optionally serves a DIFFERENT digest's bytes
// (serveDigestOverride) to exercise the puller's verify-on-the-fly.
type staticTokenVerifierStub struct {
	token               string
	digest              string
	serveDigestOverride string
}

func (s staticTokenVerifierStub) Verify(token, digest string) (string, bool) {
	if token != s.token || digest != s.digest {
		return "", false
	}
	if s.serveDigestOverride != "" {
		return s.serveDigestOverride, true
	}
	return digest, true
}

// authorisedGet issues a token-authorised GET against the serve handler and
// returns the response. The serve handler runs without TLS here (httptest.NewServer);
// the node-leaf CN gate is permissive when no peer cert is present.
func authorisedGet(t *testing.T, handler interface {
	Verify(token, digest string) (string, bool)
}, store Getter, token, digest string,
) *http.Response {
	t.Helper()
	srv := httptest.NewServer(NewServeHandler(store, handler))
	t.Cleanup(srv.Close)
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/blobs/"+digest, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	return resp
}

func TestServeResolvesImageStore(t *testing.T) {
	content := []byte("image-cache-only-blob")
	d := digestOf(content)

	art, err := artifactstore.NewForTesting(t.TempDir())
	if err != nil {
		t.Fatalf("art store: %v", err)
	}
	img, err := artifactstore.NewForTesting(t.TempDir())
	if err != nil {
		t.Fatalf("img store: %v", err)
	}
	// Blob lives ONLY in the image cache tier.
	if err := img.Put(d, bytes.NewReader(content)); err != nil {
		t.Fatalf("seed img: %v", err)
	}

	const token = "otx_pull_img"
	resolver := MultiStore(art, img)
	resp := authorisedGet(t, staticTokenVerifierStub{token: token, digest: d}, resolver, token, d)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET image-only digest status = %d, want 200", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, content) {
		t.Errorf("body = %q, want %q", got, content)
	}
}

func TestServeStillServesArtifactStore(t *testing.T) {
	content := []byte("artifact-store-blob")
	d := digestOf(content)

	art, err := artifactstore.NewForTesting(t.TempDir())
	if err != nil {
		t.Fatalf("art store: %v", err)
	}
	img, err := artifactstore.NewForTesting(t.TempDir())
	if err != nil {
		t.Fatalf("img store: %v", err)
	}
	// Blob lives ONLY in the artifact store.
	if err := art.Put(d, bytes.NewReader(content)); err != nil {
		t.Fatalf("seed art: %v", err)
	}

	const token = "otx_pull_art"
	resolver := MultiStore(art, img)
	resp := authorisedGet(t, staticTokenVerifierStub{token: token, digest: d}, resolver, token, d)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET artifact digest status = %d, want 200", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, content) {
		t.Errorf("body = %q, want %q", got, content)
	}
}

func TestServeAndPullRoundtrip(t *testing.T) {
	content := []byte("peer-blob-content")
	d := digestOf(content)

	srcStore, err := artifactstore.NewForTesting(t.TempDir())
	if err != nil {
		t.Fatalf("src store: %v", err)
	}
	if err := srcStore.Put(d, bytes.NewReader(content)); err != nil {
		t.Fatalf("seed src: %v", err)
	}

	const token = "otx_pull_test"
	h := NewServeHandler(srcStore, staticTokenVerifierStub{token: token, digest: d})
	srv := httptest.NewTLSServer(h)
	defer srv.Close()

	dstStore, err := artifactstore.NewForTesting(t.TempDir())
	if err != nil {
		t.Fatalf("dst store: %v", err)
	}
	if err := Pull(context.Background(), PullArgs{
		Endpoint:  srv.URL,
		Digest:    d,
		Token:     token,
		Store:     dstStore,
		TLSClient: srv.Client(),
	}); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if !dstStore.Has(d) {
		t.Errorf("dst store missing blob after pull")
	}
}

func TestPullBadTokenRejected(t *testing.T) {
	content := []byte("guarded")
	d := digestOf(content)
	srcStore, _ := artifactstore.NewForTesting(t.TempDir())
	_ = srcStore.Put(d, bytes.NewReader(content))

	h := NewServeHandler(srcStore, staticTokenVerifierStub{token: "right", digest: d})
	srv := httptest.NewTLSServer(h)
	defer srv.Close()

	dstStore, _ := artifactstore.NewForTesting(t.TempDir())
	err := Pull(context.Background(), PullArgs{
		Endpoint: srv.URL, Digest: d, Token: "wrong", Store: dstStore, TLSClient: srv.Client(),
	})
	if err == nil {
		t.Fatalf("Pull with wrong token = nil, want 401/403 error")
	}
	if dstStore.Has(d) {
		t.Errorf("blob materialized despite bad token")
	}
}

func TestPullDigestMismatchRejected(t *testing.T) {
	content := []byte("served-bytes")
	served := digestOf(content)
	claimed := digestOf([]byte("a-different-digest"))

	srcStore, _ := artifactstore.NewForTesting(t.TempDir())
	_ = srcStore.Put(served, bytes.NewReader(content))

	// Authorize the CLAIMED digest but serve SERVED's bytes (a buggy/malicious
	// holder): the puller asks for `claimed`, the server streams `served` content;
	// the puller's verify-on-the-fly must reject (the bytes hash to served, not claimed).
	h := NewServeHandler(srcStore, staticTokenVerifierStub{token: "t", digest: claimed, serveDigestOverride: served})
	srv := httptest.NewTLSServer(h)
	defer srv.Close()

	dstStore, _ := artifactstore.NewForTesting(t.TempDir())
	err := Pull(context.Background(), PullArgs{
		Endpoint: srv.URL, Digest: claimed, Token: "t", Store: dstStore, TLSClient: srv.Client(),
	})
	if !errors.Is(err, ErrBlobVerifyFailed) {
		t.Errorf("Pull digest-mismatch err = %v, want ErrBlobVerifyFailed", err)
	}
	if dstStore.Has(claimed) {
		t.Errorf("mismatched blob materialized")
	}
}
