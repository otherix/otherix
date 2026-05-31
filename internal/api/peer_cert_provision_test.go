// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package api_test

import (
	"bytes"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/api"
	"github.com/otherix/otherix/internal/auth"
)

func genCAMaterial(t *testing.T) auth.ClusterCAResult {
	t.Helper()
	ca, err := auth.GenerateClusterCA(time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("GenerateClusterCA: %v", err)
	}
	return ca
}

func TestProvisionPeerCertAutoGenerate(t *testing.T) {
	dir := t.TempDir()
	ca := genCAMaterial(t)
	params := api.PeerCertParams{
		PeerURL:     "https://10.0.0.10:2380",
		GenCertPath: filepath.Join(dir, "peer.crt"),
		GenKeyPath:  filepath.Join(dir, "peer.key"),
		GenCAPath:   filepath.Join(dir, "ca.crt"),
	}

	mat, err := api.ProvisionPeerCert(ca, params, time.Now(), discardLogger())
	if err != nil {
		t.Fatalf("ProvisionPeerCert(auto): %v", err)
	}
	if mat.Source != "auto_generate" {
		t.Errorf("Source = %q, want auto_generate", mat.Source)
	}
	if mat.CertFile != params.GenCertPath || mat.KeyFile != params.GenKeyPath || mat.CAFile != params.GenCAPath {
		t.Errorf("returned paths = %+v, want the gen paths", mat)
	}

	// CA trust file must equal the on-disk CA cert PEM.
	caOnDisk, err := os.ReadFile(params.GenCAPath)
	if err != nil {
		t.Fatalf("read ca file: %v", err)
	}
	if !bytes.Equal(caOnDisk, ca.CertPEM) {
		t.Errorf("peer CA trust file differs from the cluster CA cert")
	}

	// Peer cert must chain to the cluster CA.
	certPEM, err := os.ReadFile(params.GenCertPath)
	if err != nil {
		t.Fatalf("read peer cert: %v", err)
	}
	leaf, _, err := auth.ParseClusterCACert(certPEM)
	if err != nil {
		t.Fatalf("parse peer cert: %v", err)
	}
	caCert, _, err := auth.ParseClusterCACert(ca.CertPEM)
	if err != nil {
		t.Fatalf("parse ca cert: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("peer cert does not chain to the cluster CA: %v", err)
	}

	// File modes: cert/ca world-readable, key owner-only.
	assertMode(t, params.GenCertPath, 0o644)
	assertMode(t, params.GenKeyPath, 0o600)
	assertMode(t, params.GenCAPath, 0o644)
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %o, want %o", path, got, want)
	}
}

func TestProvisionPeerCertOperatorFiles(t *testing.T) {
	dir := t.TempDir()
	ca := genCAMaterial(t)
	gen := api.PeerCertParams{
		PeerURL:     "https://10.0.0.10:2380",
		GenCertPath: filepath.Join(dir, "peer.crt"),
		GenKeyPath:  filepath.Join(dir, "peer.key"),
		GenCAPath:   filepath.Join(dir, "ca.crt"),
	}
	// Seed operator-provided material by generating once, then point the
	// operator override fields at those files.
	if _, err := api.ProvisionPeerCert(ca, gen, time.Now(), discardLogger()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	other := t.TempDir()
	params := api.PeerCertParams{
		PeerURL:          "https://10.0.0.10:2380",
		OperatorCertFile: gen.GenCertPath,
		OperatorKeyFile:  gen.GenKeyPath,
		OperatorCAFile:   gen.GenCAPath,
		GenCertPath:      filepath.Join(other, "peer.crt"),
		GenKeyPath:       filepath.Join(other, "peer.key"),
		GenCAPath:        filepath.Join(other, "ca.crt"),
	}
	mat, err := api.ProvisionPeerCert(ca, params, time.Now(), discardLogger())
	if err != nil {
		t.Fatalf("ProvisionPeerCert(operator): %v", err)
	}
	if mat.Source != "operator_files" {
		t.Errorf("Source = %q, want operator_files", mat.Source)
	}
	if mat.CertFile != gen.GenCertPath {
		t.Errorf("CertFile = %q, want the operator path %q", mat.CertFile, gen.GenCertPath)
	}
	// The gen paths must NOT have been written - operator override skips
	// auto-generation entirely.
	if _, err := os.Stat(params.GenCertPath); !os.IsNotExist(err) {
		t.Errorf("auto-gen path was written despite operator override")
	}
}

func TestProvisionPeerCertOperatorFilesMissing(t *testing.T) {
	dir := t.TempDir()
	ca := genCAMaterial(t)
	params := api.PeerCertParams{
		PeerURL:          "https://10.0.0.10:2380",
		OperatorCertFile: filepath.Join(dir, "nonexistent.crt"),
		OperatorKeyFile:  filepath.Join(dir, "nonexistent.key"),
		OperatorCAFile:   filepath.Join(dir, "nonexistent-ca.crt"),
	}
	if _, err := api.ProvisionPeerCert(ca, params, time.Now(), discardLogger()); err == nil {
		t.Fatal("expected error when operator peer files are configured but missing, got nil")
	}
}
