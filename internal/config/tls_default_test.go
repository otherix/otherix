// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTempConfig writes body to a temp api.yaml and returns its path.
func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "api.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

func TestServerTLSEnabledDefaultsTrue(t *testing.T) {
	// A config that omits server.tls.* must resolve to enabled.
	path := writeTempConfig(t, "server:\n  listen: \"0.0.0.0:8080\"\nauth:\n  jwt_secret: \"from-file-padded-to-32-byte-min!\"\n")
	cfg, err := LoadAPI(path)
	if err != nil {
		t.Fatalf("LoadAPI: %v", err)
	}
	if !cfg.Server.TLS.Enabled {
		t.Errorf("Server.TLS.Enabled = false, want true (absent key must default on)")
	}
}

func TestServerTLSEnabledExplicitFalse(t *testing.T) {
	path := writeTempConfig(t, "server:\n  listen: \"0.0.0.0:8080\"\n  tls:\n    enabled: false\nauth:\n  jwt_secret: \"from-file-padded-to-32-byte-min!\"\n")
	cfg, err := LoadAPI(path)
	if err != nil {
		t.Fatalf("LoadAPI: %v", err)
	}
	if cfg.Server.TLS.Enabled {
		t.Errorf("Server.TLS.Enabled = true, want false (explicit opt-out)")
	}
}
