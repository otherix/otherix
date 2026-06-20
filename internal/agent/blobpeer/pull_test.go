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

// TestEffectivePullCap proves the cap selection: a positive ExpectedSize bounds
// at ExpectedSize+1 (the honest blob plus the trip byte), while an unsized pull
// (<= 0) falls back to the absolute cap so it can never run unbounded.
func TestEffectivePullCap(t *testing.T) {
	cases := []struct {
		name     string
		expected int64
		want     int64
	}{
		{"sized", 4096, 4097},
		{"zero-falls-back", 0, maxUnsizedPullBytes},
		{"negative-falls-back", -1, maxUnsizedPullBytes},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := effectivePullCap(c.expected); got != c.want {
				t.Errorf("effectivePullCap(%d) = %d, want %d", c.expected, got, c.want)
			}
		})
	}
}

// TestPullUnsizedFallsBackToCap proves an unsized pull (ExpectedSize <= 0) is
// bounded by the absolute fallback cap rather than running with no bound. The
// const is too large to stream in a test, so this drives the boundedReader
// directly at the fallback cap and asserts the cap is selected and enforced.
func TestPullUnsizedFallsBackToCap(t *testing.T) {
	if effectivePullCap(0) != maxUnsizedPullBytes {
		t.Fatalf("unsized cap = %d, want %d", effectivePullCap(0), maxUnsizedPullBytes)
	}
	// A boundedReader at the fallback cap still trips on overflow: stream one byte
	// past a tiny stand-in cap to confirm the bound is real, not a no-op.
	const cap = 4
	br := &boundedReader{r: bytes.NewReader(bytes.Repeat([]byte("x"), cap+1)), max: cap}
	if _, err := io.ReadAll(br); !errors.Is(err, ErrBlobTooLarge) {
		t.Fatalf("bounded read err = %v, want ErrBlobTooLarge", err)
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
