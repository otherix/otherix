// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/auth"
)

func TestLoadOrGenerateClusterCAOnDiskGenerates(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cluster-ca.crt")
	keyPath := filepath.Join(dir, "cluster-ca.key")
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)

	res, generated, err := auth.LoadOrGenerateClusterCAOnDisk(certPath, keyPath, true, now)
	if err != nil {
		t.Fatalf("LoadOrGenerateClusterCAOnDisk(absent, allowGenerate) error: %v", err)
	}
	if !generated {
		t.Errorf("generated = false, want true on a fresh provision")
	}
	if len(res.CertPEM) == 0 || len(res.KeyPEM) == 0 {
		t.Fatalf("result missing cert/key material: cert=%d key=%d bytes", len(res.CertPEM), len(res.KeyPEM))
	}
	if !res.NotBefore.Equal(now.UTC()) {
		t.Errorf("NotBefore = %v, want %v", res.NotBefore, now.UTC())
	}

	// Files must exist on disk and round-trip back to the same cert.
	cert, der, err := auth.ParseClusterCACert(res.CertPEM)
	if err != nil {
		t.Fatalf("ParseClusterCACert(result): %v", err)
	}
	if cert.Subject.CommonName != "otherix-cluster-ca" {
		t.Errorf("CN = %q, want otherix-cluster-ca", cert.Subject.CommonName)
	}
	onDisk, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read written cert: %v", err)
	}
	if !bytes.Equal(onDisk, res.CertPEM) {
		t.Errorf("on-disk cert PEM differs from returned material")
	}
	_ = der
}

func TestLoadOrGenerateClusterCAOnDiskLoadsExisting(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cluster-ca.crt")
	keyPath := filepath.Join(dir, "cluster-ca.key")
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)

	first, _, err := auth.LoadOrGenerateClusterCAOnDisk(certPath, keyPath, true, now)
	if err != nil {
		t.Fatalf("first provision: %v", err)
	}

	// A later call with a different clock must NOT regenerate; it loads the
	// persisted CA verbatim, so the fingerprint and validity are unchanged.
	later := now.Add(48 * time.Hour)
	second, generated, err := auth.LoadOrGenerateClusterCAOnDisk(certPath, keyPath, true, later)
	if err != nil {
		t.Fatalf("second provision: %v", err)
	}
	if generated {
		t.Errorf("generated = true on reload, want false")
	}
	if !bytes.Equal(first.Fingerprint, second.Fingerprint) {
		t.Errorf("fingerprint changed across reload: first=%x second=%x", first.Fingerprint, second.Fingerprint)
	}
	if !first.NotAfter.Equal(second.NotAfter) {
		t.Errorf("NotAfter changed across reload: first=%v second=%v", first.NotAfter, second.NotAfter)
	}
}

func TestLoadOrGenerateClusterCAOnDiskAbsentWithoutGenerate(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cluster-ca.crt")
	keyPath := filepath.Join(dir, "cluster-ca.key")
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)

	_, _, err := auth.LoadOrGenerateClusterCAOnDisk(certPath, keyPath, false, now)
	if !errors.Is(err, auth.ErrClusterCAAbsent) {
		t.Errorf("error = %v, want ErrClusterCAAbsent", err)
	}
}

func TestLoadOrGenerateClusterCAOnDiskPartialState(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cluster-ca.crt")
	keyPath := filepath.Join(dir, "cluster-ca.key")
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)

	// Cert present, key absent: corrupt/partial state, not a clean
	// "absent" — must error rather than silently regenerate.
	if _, _, err := auth.LoadOrGenerateClusterCAOnDisk(certPath, keyPath, true, now); err != nil {
		t.Fatalf("seed provision: %v", err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatalf("remove key: %v", err)
	}
	_, _, err := auth.LoadOrGenerateClusterCAOnDisk(certPath, keyPath, true, now)
	if err == nil {
		t.Fatal("expected error on cert-without-key partial state, got nil")
	}
	if errors.Is(err, auth.ErrClusterCAAbsent) {
		t.Errorf("partial state reported as ErrClusterCAAbsent; want a distinct corruption error")
	}
}

func TestLoadOrGenerateClusterCAOnDiskRejectsMismatchedPair(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cluster-ca.crt")
	keyPath := filepath.Join(dir, "cluster-ca.key")
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)

	ca1, err := auth.GenerateClusterCA(now)
	if err != nil {
		t.Fatalf("GenerateClusterCA ca1: %v", err)
	}
	ca2, err := auth.GenerateClusterCA(now)
	if err != nil {
		t.Fatalf("GenerateClusterCA ca2: %v", err)
	}
	// Cert from ca1, key from ca2: a mismatched pair on disk.
	if err := os.WriteFile(certPath, ca1.CertPEM, 0o644); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, ca2.KeyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	if _, _, err := auth.LoadOrGenerateClusterCAOnDisk(certPath, keyPath, true, now); err == nil {
		t.Fatal("expected error loading a mismatched cert/key pair, got nil")
	}
}

func TestLoadOrGenerateClusterCAOnDiskFileModes(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cluster-ca.crt")
	keyPath := filepath.Join(dir, "cluster-ca.key")
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)

	if _, _, err := auth.LoadOrGenerateClusterCAOnDisk(certPath, keyPath, true, now); err != nil {
		t.Fatalf("provision: %v", err)
	}
	certInfo, err := os.Stat(certPath)
	if err != nil {
		t.Fatalf("stat cert: %v", err)
	}
	if got := certInfo.Mode().Perm(); got != 0o644 {
		t.Errorf("cert mode = %o, want 0644", got)
	}
	keyInfo, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if got := keyInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("key mode = %o, want 0600", got)
	}
}
