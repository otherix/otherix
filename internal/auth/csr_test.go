// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth_test

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/auth"
)

// generateValidCSR returns a PEM CSR signed by a fresh ECDSA P-256
// keypair. Subject CN matches the helper's input arg — the redemption
// handler ignores it but the helper sets it for realism.
func generateValidCSR(t *testing.T, cn string) ([]byte, *ecdsa.PrivateKey) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa key: %v", err)
	}
	tmpl := &x509.CertificateRequest{
		Subject:            pkix.Name{CommonName: cn},
		SignatureAlgorithm: x509.ECDSAWithSHA256,
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, priv)
	if err != nil {
		t.Fatalf("create csr: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), priv
}

func TestValidateCSR_HappyPath_ECDSA_P256(t *testing.T) {
	t.Parallel()
	pemBytes, _ := generateValidCSR(t, "node-test")
	csr, err := auth.ValidateCSR(pemBytes)
	if err != nil {
		t.Fatalf("ValidateCSR: %v", err)
	}
	if csr == nil {
		t.Fatal("ValidateCSR returned nil csr")
	}
	if csr.Subject.CommonName != "node-test" {
		t.Errorf("Subject CN = %q, want node-test", csr.Subject.CommonName)
	}
}

func TestValidateCSR_HappyPath_ECDSA_P384(t *testing.T) {
	t.Parallel()
	priv, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa key: %v", err)
	}
	tmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: "n"}}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, priv)
	if err != nil {
		t.Fatalf("create csr: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})

	if _, err := auth.ValidateCSR(pemBytes); err != nil {
		t.Errorf("P-384 CSR rejected: %v", err)
	}
}

func TestValidateCSR_HappyPath_RSA_2048(t *testing.T) {
	t.Parallel()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	tmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: "n"}}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, priv)
	if err != nil {
		t.Fatalf("create csr: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})

	if _, err := auth.ValidateCSR(pemBytes); err != nil {
		t.Errorf("RSA 2048 CSR rejected: %v", err)
	}
}

func TestValidateCSR_Reject_EmptyOrMalformedPEM(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input []byte
	}{
		{"empty", []byte{}},
		{"garbage", []byte("not a pem")},
		{"wrong_block_type", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{0x00}})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := auth.ValidateCSR(c.input)
			if !errors.Is(err, auth.ErrCSRPEM) {
				t.Errorf("err = %v, want wraps ErrCSRPEM", err)
			}
		})
	}
}

func TestValidateCSR_Reject_TruncatedDER(t *testing.T) {
	t.Parallel()
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: []byte{0x30, 0x00}})
	_, err := auth.ValidateCSR(pemBytes)
	if !errors.Is(err, auth.ErrCSRParse) {
		t.Errorf("err = %v, want wraps ErrCSRParse", err)
	}
}

func TestValidateCSR_Reject_CorruptedSignature(t *testing.T) {
	t.Parallel()
	pemBytes, _ := generateValidCSR(t, "n")
	block, _ := pem.Decode(pemBytes)
	corrupted := make([]byte, len(block.Bytes))
	copy(corrupted, block.Bytes)
	// Flip last byte — likely in the signature segment.
	corrupted[len(corrupted)-1] ^= 0xFF
	pemBytes = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: corrupted})

	_, err := auth.ValidateCSR(pemBytes)
	// Could surface as either signature failure or parse failure depending
	// on which byte got flipped. Both are acceptable rejection paths.
	if err == nil {
		t.Fatal("corrupted signature accepted")
	}
	if !errors.Is(err, auth.ErrCSRSignature) && !errors.Is(err, auth.ErrCSRParse) {
		t.Errorf("err = %v, want wraps ErrCSRSignature or ErrCSRParse", err)
	}
}

func TestValidateCSR_Reject_RSA_1024(t *testing.T) {
	t.Parallel()
	priv, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	tmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: "n"}}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, priv)
	if err != nil {
		t.Fatalf("create csr: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})

	_, err = auth.ValidateCSR(pemBytes)
	if !errors.Is(err, auth.ErrCSRKeyType) {
		t.Errorf("err = %v, want wraps ErrCSRKeyType", err)
	}
}

func TestValidateCSR_Reject_ECDSA_P224(t *testing.T) {
	t.Parallel()
	priv, err := ecdsa.GenerateKey(elliptic.P224(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa P-224: %v", err)
	}
	tmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: "n"}}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, priv)
	if err != nil {
		t.Fatalf("create csr: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})

	_, err = auth.ValidateCSR(pemBytes)
	if !errors.Is(err, auth.ErrCSRKeyType) {
		t.Errorf("err = %v, want wraps ErrCSRKeyType", err)
	}
}

func TestValidateCSR_Reject_Ed25519(t *testing.T) {
	t.Parallel()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519: %v", err)
	}
	tmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: "n"}}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, priv)
	if err != nil {
		t.Fatalf("create csr: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})

	_, err = auth.ValidateCSR(pemBytes)
	if !errors.Is(err, auth.ErrCSRKeyType) {
		t.Errorf("err = %v, want wraps ErrCSRKeyType", err)
	}
}

func TestValidateCSR_Reject_BasicConstraints_CATrue(t *testing.T) {
	t.Parallel()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa key: %v", err)
	}
	// Encoded BasicConstraints { cA: TRUE }: 30 03 01 01 FF
	tmpl := &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "n"},
		ExtraExtensions: []pkix.Extension{
			{
				Id:       []int{2, 5, 29, 19},
				Critical: true,
				Value:    []byte{0x30, 0x03, 0x01, 0x01, 0xFF},
			},
		},
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, priv)
	if err != nil {
		t.Fatalf("create csr: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})

	_, err = auth.ValidateCSR(pemBytes)
	if !errors.Is(err, auth.ErrCSRForbidExt) {
		t.Errorf("err = %v, want wraps ErrCSRForbidExt", err)
	}
}

func TestValidateCSR_Reject_KeyUsage_CertSign(t *testing.T) {
	t.Parallel()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa key: %v", err)
	}
	// Encoded KeyUsage with keyCertSign (bit 5) set:
	// BIT STRING (3 octets) — 2 unused bits, byte = 0x04 (bit 5 MSB-first).
	// DER: 03 02 02 04 — but encoded in extension Value as raw bytes from
	// caller's perspective: the parser sees the BIT STRING starting at index 0.
	tmpl := &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "n"},
		ExtraExtensions: []pkix.Extension{
			{
				Id:       []int{2, 5, 29, 15},
				Critical: true,
				Value:    []byte{0x03, 0x02, 0x02, 0x04}, // BIT STRING with keyCertSign
			},
		},
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, priv)
	if err != nil {
		t.Fatalf("create csr: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})

	_, err = auth.ValidateCSR(pemBytes)
	if !errors.Is(err, auth.ErrCSRForbidExt) {
		t.Errorf("err = %v, want wraps ErrCSRForbidExt", err)
	}
}

func TestSignCSR_HappyPath_TemplateFieldsCorrect(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	caResult, err := auth.GenerateClusterCA(now)
	if err != nil {
		t.Fatalf("GenerateClusterCA: %v", err)
	}
	caCert, _, err := auth.ParseClusterCACert(caResult.CertPEM)
	if err != nil {
		t.Fatalf("ParseClusterCACert: %v", err)
	}
	caKey, err := auth.ParseClusterCAKey(caResult.KeyPEM)
	if err != nil {
		t.Fatalf("ParseClusterCAKey: %v", err)
	}
	signer, ok := caKey.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatalf("CA key type = %T, want *ecdsa.PrivateKey", caKey)
	}

	pemBytes, _ := generateValidCSR(t, "node-foo")
	csr, err := auth.ValidateCSR(pemBytes)
	if err != nil {
		t.Fatalf("ValidateCSR: %v", err)
	}

	certPEM, parsed, err := auth.SignCSR(csr, "foo-node", "", caCert, signer, now)
	if err != nil {
		t.Fatalf("SignCSR: %v", err)
	}

	if !strings.Contains(string(certPEM), "BEGIN CERTIFICATE") {
		t.Errorf("cert PEM missing BEGIN CERTIFICATE: %q", certPEM)
	}

	if parsed.Subject.CommonName != "node-foo-node" {
		t.Errorf("Subject CN = %q, want node-foo-node", parsed.Subject.CommonName)
	}

	wantDNS := []string{"node-foo-node.agents.otherix.local", "localhost"}
	if len(parsed.DNSNames) != len(wantDNS) {
		t.Errorf("DNSNames = %v, want %v", parsed.DNSNames, wantDNS)
	} else {
		for i := range wantDNS {
			if parsed.DNSNames[i] != wantDNS[i] {
				t.Errorf("DNSNames[%d] = %q, want %q", i, parsed.DNSNames[i], wantDNS[i])
			}
		}
	}

	foundLoopback := false
	for _, ip := range parsed.IPAddresses {
		if ip.Equal(net.ParseIP("127.0.0.1")) {
			foundLoopback = true
			break
		}
	}
	if !foundLoopback {
		t.Error("IPAddresses missing 127.0.0.1")
	}

	if parsed.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		t.Error("KeyUsage missing DigitalSignature")
	}
	if parsed.KeyUsage&x509.KeyUsageKeyEncipherment == 0 {
		t.Error("KeyUsage missing KeyEncipherment")
	}

	hasServerAuth := false
	hasClientAuth := false
	for _, eku := range parsed.ExtKeyUsage {
		if eku == x509.ExtKeyUsageServerAuth {
			hasServerAuth = true
		}
		if eku == x509.ExtKeyUsageClientAuth {
			hasClientAuth = true
		}
	}
	if !hasServerAuth {
		t.Error("ExtKeyUsage missing ServerAuth")
	}
	if !hasClientAuth {
		t.Error("ExtKeyUsage missing ClientAuth")
	}

	// Validity period — NotBefore is 1m back for skew, NotAfter is 1y forward.
	if !parsed.NotBefore.Before(now) {
		t.Errorf("NotBefore = %v, want < %v", parsed.NotBefore, now)
	}
	wantNotAfter := now.Add(auth.LeafCertValidity)
	if !parsed.NotAfter.Equal(wantNotAfter) {
		t.Errorf("NotAfter = %v, want %v", parsed.NotAfter, wantNotAfter)
	}

	// Serial — positive, less than 2^64.
	if parsed.SerialNumber.Sign() <= 0 {
		t.Errorf("Serial = %v, want positive", parsed.SerialNumber)
	}
}

func TestSignCSR_ChainVerifies_AgainstCA(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	caResult, err := auth.GenerateClusterCA(now)
	if err != nil {
		t.Fatalf("GenerateClusterCA: %v", err)
	}
	caCert, _, err := auth.ParseClusterCACert(caResult.CertPEM)
	if err != nil {
		t.Fatalf("ParseClusterCACert: %v", err)
	}
	caKey, err := auth.ParseClusterCAKey(caResult.KeyPEM)
	if err != nil {
		t.Fatalf("ParseClusterCAKey: %v", err)
	}
	signer := caKey.(*ecdsa.PrivateKey)

	pemBytes, _ := generateValidCSR(t, "node-chain")
	csr, _ := auth.ValidateCSR(pemBytes)

	_, leafCert, err := auth.SignCSR(csr, "chain-node", "", caCert, signer, now)
	if err != nil {
		t.Fatalf("SignCSR: %v", err)
	}

	// Verify chain.
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	opts := x509.VerifyOptions{
		Roots:       roots,
		CurrentTime: now.Add(time.Hour),
		// Both ServerAuth and ClientAuth set — verifier picks one.
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	if _, err := leafCert.Verify(opts); err != nil {
		t.Errorf("leaf cert chain verification failed: %v", err)
	}
}

func TestSignCSR_DistinctSerials(t *testing.T) {
	t.Parallel()
	now := time.Now()
	caResult, _ := auth.GenerateClusterCA(now)
	caCert, _, _ := auth.ParseClusterCACert(caResult.CertPEM)
	caKey, _ := auth.ParseClusterCAKey(caResult.KeyPEM)
	signer := caKey.(*ecdsa.PrivateKey)

	pemBytes, _ := generateValidCSR(t, "n")
	csr, _ := auth.ValidateCSR(pemBytes)

	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		_, leaf, err := auth.SignCSR(csr, "x", "", caCert, signer, now)
		if err != nil {
			t.Fatalf("SignCSR #%d: %v", i, err)
		}
		key := leaf.SerialNumber.String()
		if seen[key] {
			t.Errorf("duplicate serial after %d signings: %s", i+1, key)
		}
		seen[key] = true
	}
}

func TestSignGatewayCSR_GatewayIdentity(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	caResult, err := auth.GenerateClusterCA(now)
	if err != nil {
		t.Fatalf("GenerateClusterCA: %v", err)
	}
	caCert, _, err := auth.ParseClusterCACert(caResult.CertPEM)
	if err != nil {
		t.Fatalf("ParseClusterCACert: %v", err)
	}
	caKey, err := auth.ParseClusterCAKey(caResult.KeyPEM)
	if err != nil {
		t.Fatalf("ParseClusterCAKey: %v", err)
	}
	signer, ok := caKey.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatalf("CA key type = %T, want *ecdsa.PrivateKey", caKey)
	}

	pemBytes, _ := generateValidCSR(t, "gateway-edge1")
	csr, err := auth.ValidateCSR(pemBytes)
	if err != nil {
		t.Fatalf("ValidateCSR: %v", err)
	}

	certPEM, parsed, err := auth.SignGatewayCSR(csr, "edge1", "https://10.77.0.5:9443", caCert, signer, now)
	if err != nil {
		t.Fatalf("SignGatewayCSR: %v", err)
	}

	if !strings.Contains(string(certPEM), "BEGIN CERTIFICATE") {
		t.Errorf("cert PEM missing BEGIN CERTIFICATE: %q", certPEM)
	}
	if parsed.Subject.CommonName != "gateway-edge1" {
		t.Errorf("Subject CN = %q, want gateway-edge1", parsed.Subject.CommonName)
	}
	if !hasDNS(parsed.DNSNames, "gateway-edge1.agents.otherix.local") {
		t.Errorf("DNSNames = %v, want gateway-edge1.agents.otherix.local", parsed.DNSNames)
	}
	if hasDNS(parsed.DNSNames, "node-edge1.agents.otherix.local") {
		t.Errorf("DNSNames = %v, must not carry a node- SAN", parsed.DNSNames)
	}
	// The advertised endpoint host must reach the cert SAN so the CP can dial
	// the gateway at its real address.
	if !hasIP(parsed.IPAddresses, "10.77.0.5") {
		t.Errorf("IPAddresses = %v, want 10.77.0.5 from advertised endpoint", parsed.IPAddresses)
	}

	// The gateway leaf chains to the cluster CA.
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	if _, err := parsed.Verify(x509.VerifyOptions{
		Roots:       roots,
		CurrentTime: now.Add(time.Hour),
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("gateway leaf chain verification failed: %v", err)
	}
}

func hasDNS(list []string, want string) bool {
	for _, n := range list {
		if strings.EqualFold(n, want) {
			return true
		}
	}
	return false
}

func hasIP(list []net.IP, want string) bool {
	target := net.ParseIP(want)
	for _, ip := range list {
		if ip.Equal(target) {
			return true
		}
	}
	return false
}

func TestSignCSRAdvertisedEndpointSAN(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	caResult, err := auth.GenerateClusterCA(now)
	if err != nil {
		t.Fatalf("GenerateClusterCA: %v", err)
	}
	caCert, _, err := auth.ParseClusterCACert(caResult.CertPEM)
	if err != nil {
		t.Fatalf("ParseClusterCACert: %v", err)
	}
	caKey, err := auth.ParseClusterCAKey(caResult.KeyPEM)
	if err != nil {
		t.Fatalf("ParseClusterCAKey: %v", err)
	}
	signer, ok := caKey.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatalf("CA key type = %T, want *ecdsa.PrivateKey", caKey)
	}

	pemBytes, _ := generateValidCSR(t, "node-1")
	csr, err := auth.ValidateCSR(pemBytes)
	if err != nil {
		t.Fatalf("ValidateCSR: %v", err)
	}

	tests := []struct {
		name     string
		endpoint string
		wantIP   string
		wantDNS  string
		extraIPs int
		extraDNS int
	}{
		{name: "ip endpoint", endpoint: "https://10.77.0.1:9443", wantIP: "10.77.0.1", extraIPs: 1, extraDNS: 0},
		{name: "dns endpoint", endpoint: "https://node1.example.com:9443", wantDNS: "node1.example.com", extraIPs: 0, extraDNS: 1},
		{name: "loopback dedup", endpoint: "https://127.0.0.1:9443", extraIPs: 0, extraDNS: 0},
		{name: "empty endpoint", endpoint: "", extraIPs: 0, extraDNS: 0},
		{name: "malformed endpoint", endpoint: "://nonsense", extraIPs: 0, extraDNS: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, cert, err := auth.SignCSR(csr, "node-1", tt.endpoint, caCert, signer, now)
			if err != nil {
				t.Fatalf("SignCSR(%q) error: %v", tt.endpoint, err)
			}
			if !hasDNS(cert.DNSNames, "node-node-1.agents.otherix.local") || !hasDNS(cert.DNSNames, "localhost") {
				t.Errorf("SignCSR(%q) DNSNames = %v, missing fixed entries", tt.endpoint, cert.DNSNames)
			}
			if !hasIP(cert.IPAddresses, "127.0.0.1") {
				t.Errorf("SignCSR(%q) IPAddresses = %v, missing 127.0.0.1", tt.endpoint, cert.IPAddresses)
			}
			if got := len(cert.IPAddresses) - 1; got != tt.extraIPs { // minus the fixed 127.0.0.1
				t.Errorf("SignCSR(%q) extra IPAddresses = %d, want %d (%v)", tt.endpoint, got, tt.extraIPs, cert.IPAddresses)
			}
			if got := len(cert.DNSNames) - 2; got != tt.extraDNS { // minus the 2 fixed DNS entries
				t.Errorf("SignCSR(%q) extra DNSNames = %d, want %d (%v)", tt.endpoint, got, tt.extraDNS, cert.DNSNames)
			}
			if tt.wantIP != "" && !hasIP(cert.IPAddresses, tt.wantIP) {
				t.Errorf("SignCSR(%q) IPAddresses = %v, want %s", tt.endpoint, cert.IPAddresses, tt.wantIP)
			}
			if tt.wantDNS != "" && !hasDNS(cert.DNSNames, tt.wantDNS) {
				t.Errorf("SignCSR(%q) DNSNames = %v, want %s", tt.endpoint, cert.DNSNames, tt.wantDNS)
			}
		})
	}
}
