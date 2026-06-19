// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package blobpeer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/otherix/otherix/internal/agent/artifactstore"
)

func TestBoundedReader(t *testing.T) {
	const max = 4
	br := &boundedReader{r: bytes.NewReader([]byte("0123456789")), max: max}
	got, err := io.ReadAll(br)
	if !errors.Is(err, ErrBlobTooLarge) {
		t.Fatalf("ReadAll err = %v, want ErrBlobTooLarge", err)
	}
	if !br.overflowed {
		t.Errorf("br.overflowed = false, want true after overflow")
	}
	if len(got) != max {
		t.Errorf("read %d bytes before tripping, want %d", len(got), max)
	}
}

func TestBoundedReaderUnderMaxPasses(t *testing.T) {
	// Pull sets max = ExpectedSize+1, so an honest body of ExpectedSize bytes
	// reads fully and the trailing EOF passes through without tripping the cap.
	const max = 11
	br := &boundedReader{r: bytes.NewReader([]byte("0123456789")), max: max}
	got, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("ReadAll err = %v, want nil", err)
	}
	if br.overflowed {
		t.Errorf("br.overflowed = true, want false for a body under the cap")
	}
	if string(got) != "0123456789" {
		t.Errorf("read = %q, want full body", got)
	}
}

// TestPullOverflowFailsClosed proves the security property: a holder streaming
// more bytes than the CP-known ExpectedSize is stopped at the bound, the pull
// fails closed, and no blob is materialized into the consumer store (the disk is
// not filled before the digest check).
func TestPullOverflowFailsClosed(t *testing.T) {
	// The blob the consumer asked for is small; the holder lies and streams a far
	// larger body. ExpectedSize is the honest (small) size the CP reported.
	expected := []byte("small-blob")
	d := digestOf(expected)
	oversized := bytes.Repeat([]byte("x"), 1<<20)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(oversized)
	}))
	defer srv.Close()

	dstStore, err := artifactstore.NewForTesting(t.TempDir())
	if err != nil {
		t.Fatalf("dst store: %v", err)
	}
	err = Pull(context.Background(), PullArgs{
		Endpoint:     srv.URL,
		Digest:       d,
		Token:        "t",
		Store:        dstStore,
		TLSClient:    srv.Client(),
		ExpectedSize: int64(len(expected)),
	})
	if !errors.Is(err, ErrBlobTooLarge) {
		t.Fatalf("Pull oversized err = %v, want ErrBlobTooLarge", err)
	}
	if dstStore.Has(d) {
		t.Errorf("blob materialized despite overflow")
	}
}

// TestPullExactExpectedSizeSucceeds proves a correctly-sized body with the right
// digest passes the bound (max = ExpectedSize+1, so the honest blob is not
// truncated).
func TestPullExactExpectedSizeSucceeds(t *testing.T) {
	content := []byte("exactly-this-blob")
	d := digestOf(content)

	srcStore, err := artifactstore.NewForTesting(t.TempDir())
	if err != nil {
		t.Fatalf("src store: %v", err)
	}
	if err := srcStore.Put(d, bytes.NewReader(content)); err != nil {
		t.Fatalf("seed src: %v", err)
	}

	const token = "otx_pull_sized"
	h := NewServeHandler(srcStore, staticTokenVerifierStub{token: token, digest: d})
	srv := httptest.NewTLSServer(h)
	defer srv.Close()

	dstStore, err := artifactstore.NewForTesting(t.TempDir())
	if err != nil {
		t.Fatalf("dst store: %v", err)
	}
	if err := Pull(context.Background(), PullArgs{
		Endpoint:     srv.URL,
		Digest:       d,
		Token:        token,
		Store:        dstStore,
		TLSClient:    srv.Client(),
		ExpectedSize: int64(len(content)),
	}); err != nil {
		t.Fatalf("Pull exact-size: %v", err)
	}
	if !dstStore.Has(d) {
		t.Errorf("dst store missing blob after exact-size pull")
	}
}
