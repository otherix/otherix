// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeCertWithCN writes a self-signed leaf cert with the given Subject CN
// to a temp file and returns its path. The key is discarded — the parse
// path only reads the Subject.
func writeCertWithCN(t *testing.T, cn string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	path := filepath.Join(t.TempDir(), "cert.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	return path
}

// gatewayConfigYAML returns a minimal gateway.yaml body that parses and
// validates, pointing tls.* at the supplied paths.
func gatewayConfigYAML(certPath, keyPath, caPath string) string {
	return "server:\n  listen: \"0.0.0.0:9443\"\n" +
		"control_plane:\n  url: \"https://cp.example:8443\"\n" +
		"migration:\n  host: \"0.0.0.0\"\n" +
		"tls:\n" +
		"  cert_path: \"" + certPath + "\"\n" +
		"  key_path: \"" + keyPath + "\"\n" +
		"  ca_cert_path: \"" + caPath + "\"\n"
}

// TestTryEnterStateB_WaitsForCertMaterial confirms the State A → B gate
// returns the config only when cert + key + CA all exist alongside the
// config; a missing cert file keeps it in State A.
func TestTryEnterStateB_WaitsForCertMaterial(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "gateway.crt")
	keyPath := filepath.Join(dir, "gateway.key")
	caPath := filepath.Join(dir, "ca.crt")
	configPath := filepath.Join(dir, "gateway.yaml")

	if err := os.WriteFile(configPath, []byte(gatewayConfigYAML(certPath, keyPath, caPath)), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Cert material absent → still in State A.
	if _, err := tryEnterStateB(configPath); err == nil {
		t.Fatalf("tryEnterStateB succeeded with no cert material, want error")
	}

	// Only cert + key present, CA missing → still in State A.
	for _, p := range []string{certPath, keyPath} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	if _, err := tryEnterStateB(configPath); err == nil {
		t.Fatalf("tryEnterStateB succeeded with CA missing, want error")
	}

	// All three present → transition, config returned.
	if err := os.WriteFile(caPath, []byte("x"), 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	cfg, err := tryEnterStateB(configPath)
	if err != nil {
		t.Fatalf("tryEnterStateB with full cert material: %v", err)
	}
	if cfg == nil {
		t.Fatal("tryEnterStateB returned nil config without error")
	}
	if cfg.ControlPlane.URL != "https://cp.example:8443" {
		t.Errorf("cfg.ControlPlane.URL = %q, want https://cp.example:8443", cfg.ControlPlane.URL)
	}
}

// TestTryEnterStateB_MissingConfig confirms a missing config file keeps the
// gateway in State A.
func TestTryEnterStateB_MissingConfig(t *testing.T) {
	if _, err := tryEnterStateB(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatalf("tryEnterStateB succeeded with no config file, want error")
	}
}

// TestParseGatewayNameFromCert_StripsPrefix confirms the parser strips the
// gateway- prefix and surfaces the operator-supplied name.
func TestParseGatewayNameFromCert_StripsPrefix(t *testing.T) {
	path := writeCertWithCN(t, "gateway-edge1")
	name, err := parseGatewayNameFromCert(path)
	if err != nil {
		t.Fatalf("parseGatewayNameFromCert: %v", err)
	}
	if name != "edge1" {
		t.Errorf("name = %q, want edge1", name)
	}
}

// TestParseGatewayNameFromCert_RejectsNodeCN confirms a hypervisor-agent
// cert (CN node-<name>) is rejected — the binaries do not share identities.
func TestParseGatewayNameFromCert_RejectsNodeCN(t *testing.T) {
	path := writeCertWithCN(t, "node-hv1")
	if _, err := parseGatewayNameFromCert(path); err == nil {
		t.Fatalf("parseGatewayNameFromCert accepted a node- CN, want rejection")
	}
}
