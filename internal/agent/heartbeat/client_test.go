// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/config"
)

// writeSelfSignedMaterial generates a throwaway self-signed ECDSA cert
// and writes cert / key / CA PEM files into a temp dir so
// buildTLSConfig can load real material without touching the network.
// The cert doubles as its own CA - buildTLSConfig only parses, it does
// not chain-verify.
func writeSelfSignedMaterial(t *testing.T) config.TLSConfig {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "heartbeat-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	dir := t.TempDir()
	cfg := config.TLSConfig{
		CertPath:   filepath.Join(dir, "tls.crt"),
		KeyPath:    filepath.Join(dir, "tls.key"),
		CACertPath: filepath.Join(dir, "ca.crt"),
	}
	for _, f := range []struct {
		path  string
		bytes []byte
	}{
		{cfg.CertPath, certPEM},
		{cfg.KeyPath, keyPEM},
		{cfg.CACertPath, certPEM},
	} {
		if err := os.WriteFile(f.path, f.bytes, 0o600); err != nil {
			t.Fatalf("write %s: %v", f.path, err)
		}
	}
	return cfg
}

// TestBuildTLSConfigPinsTLS13Floor asserts the agent->CP heartbeat
// dialer refuses handshakes below TLS 1.3. Both ends are Go crypto/tls
// (1.3 always available), so there is no compatibility reason to keep
// a 1.2 floor.
func TestBuildTLSConfigPinsTLS13Floor(t *testing.T) {
	tlsCfg, err := buildTLSConfig(writeSelfSignedMaterial(t))
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if tlsCfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %#x, want %#x (TLS 1.3)", tlsCfg.MinVersion, uint16(tls.VersionTLS13))
	}
}
