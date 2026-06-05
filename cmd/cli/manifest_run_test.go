// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateRejectsDirectoryFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("no HTTP for a bad -f path")
	}))
	defer srv.Close()
	dir := t.TempDir() // a directory, not a regular file
	_, _, err := runRoot(t, srv.URL, "create", "-f", dir)
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("err = %v, want 'not a regular file'", err)
	}
}

func TestCreateRejectsOversizeManifest(t *testing.T) {
	prev := maxManifestBytes
	maxManifestBytes = 16
	t.Cleanup(func() { maxManifestBytes = prev })
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("no HTTP for an oversize manifest")
	}))
	defer srv.Close()
	big := filepath.Join(t.TempDir(), "big.yaml")
	if err := os.WriteFile(big, []byte(strings.Repeat("a", 64)), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, _, err := runRoot(t, srv.URL, "create", "-f", big)
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("err = %v, want size-limit error", err)
	}
}

func TestCreateRejectsDoubleStdin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("no HTTP when -f - is repeated")
	}))
	defer srv.Close()
	// runRootStdin (in create_test.go) sets a strings.Reader stdin (not a
	// TTY), so the terminal-refusal branch is skipped; the at-most-once
	// guard fires first.
	_, _, err := runRootStdin(t, srv.URL, "irrelevant", "create", "-f", "-", "-f", "-")
	if err == nil || !strings.Contains(err.Error(), "at most once") {
		t.Errorf("err = %v, want '-f - may be given at most once'", err)
	}
}
