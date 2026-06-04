// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package wgkey manages the agent's persistent WireGuard private key. The key
// is generated lazily on first serve and reused thereafter; the agent never
// re-bootstraps to obtain it.
package wgkey

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// LoadOrGenerateKey returns the agent's WireGuard private key. On first call
// (no file at path) it generates a fresh Curve25519 key, persists it 0600, and
// returns it; subsequent calls load and return the existing key. The parent
// directory is created 0700 when absent. The on-disk form is the wgtypes
// base64 string. A parse failure on an existing file is returned as an error
// rather than silently regenerated - a fresh key would orphan the CP-side
// pubkey guard and strand the agent's overlay address.
func LoadOrGenerateKey(path string) (wgtypes.Key, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path is operator-configured (agent.yaml wireguard.private_key_path)
	switch {
	case err == nil:
		key, perr := wgtypes.ParseKey(strings.TrimSpace(string(raw)))
		if perr != nil {
			return wgtypes.Key{}, fmt.Errorf("parse wireguard key %s: %v", path, perr)
		}
		return key, nil
	case os.IsNotExist(err):
		return generateAndPersist(path)
	default:
		return wgtypes.Key{}, fmt.Errorf("read wireguard key %s: %v", path, err)
	}
}

// generateAndPersist mints a fresh key and writes it atomically (temp file in
// the same directory + rename) so a crash mid-write never leaves a truncated
// key on disk.
func generateAndPersist(path string) (wgtypes.Key, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return wgtypes.Key{}, fmt.Errorf("create wireguard key dir: %v", err)
	}
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return wgtypes.Key{}, fmt.Errorf("generate wireguard key: %v", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".private.key-*")
	if err != nil {
		return wgtypes.Key{}, fmt.Errorf("create temp wireguard key: %v", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return wgtypes.Key{}, fmt.Errorf("chmod temp wireguard key: %v", err)
	}
	if _, err := tmp.WriteString(key.String()); err != nil {
		_ = tmp.Close()
		return wgtypes.Key{}, fmt.Errorf("write temp wireguard key: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return wgtypes.Key{}, fmt.Errorf("close temp wireguard key: %v", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return wgtypes.Key{}, fmt.Errorf("rename wireguard key into place: %v", err)
	}
	return key, nil
}
