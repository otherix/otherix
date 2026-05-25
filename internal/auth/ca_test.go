// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/auth"
)

// TestGenerateClusterCA_ParametersHonored verifies the cert template
// matches the cluster CA contract: ECDSA P-384, BasicConstraints
// CA:TRUE + MaxPathLen:0, KeyUsage certSign|crlSign|digitalSignature,
// validity 10y, subject CN=otherix-cluster-ca, self-signed.
func TestGenerateClusterCA_ParametersHonored(t *testing.T) {
	now := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	result, err := auth.GenerateClusterCA(now)
	if err != nil {
		t.Fatalf("GenerateClusterCA: %v", err)
	}

	if !result.NotBefore.Equal(now.UTC()) {
		t.Errorf("NotBefore = %v, want %v", result.NotBefore, now.UTC())
	}
	want := now.UTC().Add(auth.ClusterCAValidity)
	if !result.NotAfter.Equal(want) {
		t.Errorf("NotAfter = %v, want %v", result.NotAfter, want)
	}

	cert, der, err := auth.ParseClusterCACert(result.CertPEM)
	if err != nil {
		t.Fatalf("ParseClusterCACert: %v", err)
	}
	if cert.Subject.CommonName != "otherix-cluster-ca" {
		t.Errorf("Subject CN = %q, want otherix-cluster-ca", cert.Subject.CommonName)
	}
	if !cert.IsCA {
		t.Error("IsCA = false, want true")
	}
	if !cert.BasicConstraintsValid {
		t.Error("BasicConstraintsValid = false, want true")
	}
	if !cert.MaxPathLenZero {
		t.Error("MaxPathLenZero = false, want true")
	}
	if cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Error("KeyUsage missing CertSign")
	}
	if cert.KeyUsage&x509.KeyUsageCRLSign == 0 {
		t.Error("KeyUsage missing CRLSign")
	}
	if cert.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		t.Error("KeyUsage missing DigitalSignature")
	}

	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("PublicKey type = %T, want *ecdsa.PublicKey", cert.PublicKey)
	}
	if pub.Curve != elliptic.P384() {
		t.Errorf("Curve = %v, want P-384", pub.Curve)
	}

	// Self-signed: verify signature using the cert's own pubkey.
	if err := cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature); err != nil {
		t.Errorf("self-signature check failed: %v", err)
	}

	// Fingerprint matches sha256(DER).
	wantFP := sha256.Sum256(der)
	if string(result.Fingerprint) != string(wantFP[:]) {
		t.Errorf("Fingerprint != sha256(cert.Raw): %x vs %x", result.Fingerprint, wantFP)
	}
}

// TestParseClusterCAKey_RoundTrip verifies the PKCS#8 private-key
// encoding survives a parse/marshal round-trip — Step 2 CSR signing
// path uses this to load the key from ca_certs.key_pem.
func TestParseClusterCAKey_RoundTrip(t *testing.T) {
	result, err := auth.GenerateClusterCA(time.Now())
	if err != nil {
		t.Fatalf("GenerateClusterCA: %v", err)
	}
	key, err := auth.ParseClusterCAKey(result.KeyPEM)
	if err != nil {
		t.Fatalf("ParseClusterCAKey: %v", err)
	}
	if _, ok := key.(*ecdsa.PrivateKey); !ok {
		t.Errorf("parsed key type = %T, want *ecdsa.PrivateKey", key)
	}
}
