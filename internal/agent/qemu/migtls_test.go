// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package qemu

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMaterializeMigrationCreds(t *testing.T) {
	pki := t.TempDir()
	ca := filepath.Join(pki, "ca.crt")
	crt := filepath.Join(pki, "node.crt")
	key := filepath.Join(pki, "node.key")
	for _, p := range []string{ca, crt, key} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name      string
		role      MigrationTLSRole
		wantFiles []string
	}{
		{name: "server", role: MigrationTLSServer, wantFiles: []string{"ca-cert.pem", "server-cert.pem", "server-key.pem"}},
		{name: "client", role: MigrationTLSClient, wantFiles: []string{"ca-cert.pem", "client-cert.pem", "client-key.pem"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "tls")
			if err := MaterializeMigrationCreds(dir, tc.role, ca, crt, key); err != nil {
				t.Fatalf("MaterializeMigrationCreds() error = %v", err)
			}
			for _, f := range tc.wantFiles {
				p := filepath.Join(dir, f)
				if _, err := os.Stat(p); err != nil {
					t.Errorf("expected %s, stat error = %v", f, err)
				}
			}
			// Server dir must not leak client-* files and vice-versa.
			forbidden := "client-cert.pem"
			if tc.role == MigrationTLSClient {
				forbidden = "server-cert.pem"
			}
			if _, err := os.Stat(filepath.Join(dir, forbidden)); err == nil {
				t.Errorf("dir contains forbidden %s for role %s", forbidden, tc.name)
			}
		})
	}
}
