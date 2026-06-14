// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package qemu

import (
	"fmt"
	"os"
	"path/filepath"
)

// MigrationTLSRole selects which QEMU tls-creds-x509 endpoint a creds dir
// is built for. The target (NBD/migration server) uses server creds; the
// source (client) uses client creds. Both reuse the node's existing PKI.
type MigrationTLSRole int

const (
	// MigrationTLSServer materializes ca-cert.pem + server-cert.pem + server-key.pem.
	MigrationTLSServer MigrationTLSRole = iota
	// MigrationTLSClient materializes ca-cert.pem + client-cert.pem + client-key.pem.
	MigrationTLSClient
)

// MaterializeMigrationCreds builds dir as a QEMU tls-creds-x509 directory
// for role, symlinking the node CA / leaf cert / leaf key into the exact
// filenames QEMU expects (ca-cert.pem, and server-* or client-*). The dir
// is created 0700. Reusing the node leaf (CN node-<name>, serverAuth +
// clientAuth EKU) for both endpoints is intentional - same trust as
// CP<->agent mTLS, no new roots.
func MaterializeMigrationCreds(dir string, role MigrationTLSRole, caPath, certPath, keyPath string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create creds dir %s: %v", dir, err)
	}

	var certName, keyName string
	switch role {
	case MigrationTLSServer:
		certName, keyName = "server-cert.pem", "server-key.pem"
	case MigrationTLSClient:
		certName, keyName = "client-cert.pem", "client-key.pem"
	default:
		return fmt.Errorf("unknown migration TLS role %d", role)
	}

	links := map[string]string{
		"ca-cert.pem": caPath,
		certName:      certPath,
		keyName:       keyPath,
	}
	for name, src := range links {
		abs, err := filepath.Abs(src)
		if err != nil {
			return fmt.Errorf("resolve %s: %v", src, err)
		}
		dst := filepath.Join(dir, name)
		_ = os.Remove(dst) // idempotent re-materialize
		if err := os.Symlink(abs, dst); err != nil {
			return fmt.Errorf("link %s -> %s: %v", dst, abs, err)
		}
	}
	return nil
}
