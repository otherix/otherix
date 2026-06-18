// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package artifactstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func newStore(t *testing.T) *Store {
	t.Helper()
	// t.TempDir is not under /var/lib/otherix, so the test store skips the
	// allowlist by constructing through newForTest.
	root := t.TempDir()
	s, err := newForTest(root)
	if err != nil {
		t.Fatalf("newForTest: %v", err)
	}
	return s
}

func TestPutHasGetRoundtrip(t *testing.T) {
	s := newStore(t)
	content := []byte("hello-blob-content")
	d := digestOf(content)

	if s.Has(d) {
		t.Fatalf("Has(%s) before Put = true, want false", d)
	}
	if err := s.Put(d, bytes.NewReader(content)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !s.Has(d) {
		t.Fatalf("Has(%s) after Put = false, want true", d)
	}
	rc, err := s.Get(d)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("Get content = %q, want %q", got, content)
	}
	sidecar := filepath.Join(s.blobsDir(), d+".sha256")
	raw, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if string(raw) != d {
		t.Errorf("sidecar = %q, want %q", raw, d)
	}
}

func TestPutVerifyMismatchRejected(t *testing.T) {
	s := newStore(t)
	content := []byte("real-content")
	wrong := digestOf([]byte("a-different-thing"))

	err := s.Put(wrong, bytes.NewReader(content))
	if err == nil {
		t.Fatalf("Put(wrong-digest) = nil, want error")
	}
	if !errors.Is(err, ErrDigestMismatch) {
		t.Errorf("Put err = %v, want ErrDigestMismatch", err)
	}
	if s.Has(wrong) {
		t.Errorf("Has(wrong) = true after a rejected Put; a mismatched blob must never be visible")
	}
	entries, _ := os.ReadDir(s.blobsDir())
	for _, e := range entries {
		if !e.IsDir() && !strings.HasSuffix(e.Name(), ".sha256") {
			t.Errorf("rejected Put left a visible blob %q", e.Name())
		}
	}
}

func TestPutIdempotentReuse(t *testing.T) {
	s := newStore(t)
	content := []byte("dedup-me")
	d := digestOf(content)
	if err := s.Put(d, bytes.NewReader(content)); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	if err := s.Put(d, bytes.NewReader(content)); err != nil {
		t.Fatalf("second Put: %v", err)
	}
}

func TestGetAbsent(t *testing.T) {
	s := newStore(t)
	_, err := s.Get(digestOf([]byte("nope")))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(absent) err = %v, want ErrNotFound", err)
	}
}

func TestDelete(t *testing.T) {
	s := newStore(t)
	content := []byte("delete-me")
	d := digestOf(content)
	if err := s.Put(d, bytes.NewReader(content)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Delete(d); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if s.Has(d) {
		t.Errorf("Has after Delete = true, want false")
	}
	if err := s.Delete(d); err != nil {
		t.Errorf("Delete(absent) = %v, want nil", err)
	}
}

func TestManifestRoundtrip(t *testing.T) {
	s := newStore(t)
	d := digestOf([]byte("manifest-key"))
	body := []byte(`{"name":"s1","disks":[]}`)
	if err := s.WriteManifest(d, body); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	got, err := s.ReadManifest(d)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("ReadManifest = %q, want %q", got, body)
	}
}

func TestListDigests(t *testing.T) {
	s := newStore(t)
	a := digestOf([]byte("a"))
	b := digestOf([]byte("bb"))
	if err := s.Put(a, bytes.NewReader([]byte("a"))); err != nil {
		t.Fatalf("Put a: %v", err)
	}
	if err := s.Put(b, bytes.NewReader([]byte("bb"))); err != nil {
		t.Fatalf("Put b: %v", err)
	}
	got, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List len = %d, want 2", len(got))
	}
	bySize := map[string]int64{}
	for _, e := range got {
		bySize[e.Digest] = e.SizeBytes
	}
	if bySize[a] != 1 || bySize[b] != 2 {
		t.Errorf("sizes = %v, want {a:1, b:2}", bySize)
	}
}

func TestNewRejectsRootOutsideAllowlist(t *testing.T) {
	if _, err := New("/etc/otherix/artifacts"); err == nil {
		t.Errorf("New(/etc/otherix/artifacts) = nil, want allowlist error")
	}
}
