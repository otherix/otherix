// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package wgkey_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/otherix/otherix/internal/agent/wgkey"
)

func TestLoadOrGenerateKey_GeneratesAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wg", "private.key")
	k1, err := wgkey.LoadOrGenerateKey(path)
	if err != nil {
		t.Fatalf("first LoadOrGenerateKey: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat persisted key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode = %o, want 600", perm)
	}
	k2, err := wgkey.LoadOrGenerateKey(path)
	if err != nil {
		t.Fatalf("second LoadOrGenerateKey: %v", err)
	}
	if k1 != k2 {
		t.Errorf("second call regenerated the key: %s != %s", k1, k2)
	}
}

func TestLoadOrGenerateKey_CorruptFileErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.key")
	if err := os.WriteFile(path, []byte("not-a-valid-key"), 0o600); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}
	if _, err := wgkey.LoadOrGenerateKey(path); err == nil {
		t.Errorf("corrupt key file: want error, got nil")
	}
}
