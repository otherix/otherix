// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package bootstrap

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
)

func TestNormalizeFingerprint(t *testing.T) {
	hex64 := strings.Repeat("a", 64)

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "bare hex lowercase", input: hex64, want: hex64},
		{name: "sha256 prefix", input: "sha256:" + hex64, want: hex64},
		{name: "uppercase normalised", input: strings.Repeat("A", 64), want: hex64},
		{name: "mixed case", input: "ABcd" + strings.Repeat("0", 60), want: "abcd" + strings.Repeat("0", 60)},
		{name: "trims whitespace", input: "  " + hex64 + "\n", want: hex64},
		{name: "trims whitespace around prefix", input: "  sha256:" + hex64 + "  ", want: hex64},
		{name: "empty", input: "", wantErr: true},
		{name: "whitespace only", input: "   ", wantErr: true},
		{name: "too short", input: strings.Repeat("a", 63), wantErr: true},
		{name: "too long", input: strings.Repeat("a", 65), wantErr: true},
		{name: "non-hex char", input: strings.Repeat("g", 64), wantErr: true},
		{name: "prefix only", input: "sha256:", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeFingerprint(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("NormalizeFingerprint(%q) error = nil, want err", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeFingerprint(%q) error = %v, want nil", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("NormalizeFingerprint(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestGenerateKeypairAndCSR(t *testing.T) {
	keyPEM, csrPEM, priv, err := generateKeypairAndCSR("test-node")
	if err != nil {
		t.Fatalf("generateKeypairAndCSR: %v", err)
	}

	if !strings.Contains(string(keyPEM), "BEGIN PRIVATE KEY") {
		t.Errorf("keyPEM missing BEGIN PRIVATE KEY: %q", string(keyPEM))
	}
	if !strings.Contains(string(csrPEM), "BEGIN CERTIFICATE REQUEST") {
		t.Errorf("csrPEM missing BEGIN CERTIFICATE REQUEST: %q", string(csrPEM))
	}

	if priv.Curve != elliptic.P384() {
		t.Errorf("priv.Curve = %v, want P-384", priv.Curve)
	}

	block, _ := pem.Decode(csrPEM)
	if block == nil {
		t.Fatal("decode csrPEM: nil block")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatalf("parse csr: %v", err)
	}

	if err := csr.CheckSignature(); err != nil {
		t.Errorf("csr.CheckSignature: %v", err)
	}

	csrPub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("csr.PublicKey type = %T, want *ecdsa.PublicKey", csr.PublicKey)
	}
	csrPubDER, err := x509.MarshalPKIXPublicKey(csrPub)
	if err != nil {
		t.Fatalf("marshal csrPub: %v", err)
	}
	privPubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal privPub: %v", err)
	}
	if !bytes.Equal(csrPubDER, privPubDER) {
		t.Error("csr.PublicKey does not match generated private key")
	}

	if got, want := csr.Subject.CommonName, "node-test-node"; got != want {
		t.Errorf("Subject.CommonName = %q, want %q", got, want)
	}

	wantDNS := map[string]bool{
		"node-test-node.agents.otherix.local": false,
		"localhost":                           false,
	}
	for _, dns := range csr.DNSNames {
		if _, ok := wantDNS[dns]; ok {
			wantDNS[dns] = true
		}
	}
	for dns, seen := range wantDNS {
		if !seen {
			t.Errorf("CSR DNSNames missing %q (have %v)", dns, csr.DNSNames)
		}
	}

	foundLoopback := false
	for _, ip := range csr.IPAddresses {
		if ip.Equal(net.ParseIP("127.0.0.1")) {
			foundLoopback = true
		}
	}
	if !foundLoopback {
		t.Errorf("CSR IPAddresses missing 127.0.0.1, got %v", csr.IPAddresses)
	}
}

func TestGenerateKeypairAndCSR_AcceptedByAuthValidateCSR(t *testing.T) {
	_, csrPEM, _, err := generateKeypairAndCSR("validate-target")
	if err != nil {
		t.Fatalf("generateKeypairAndCSR: %v", err)
	}
	csr, err := auth.ValidateCSR(csrPEM)
	if err != nil {
		t.Fatalf("auth.ValidateCSR rejected our CSR: %v", err)
	}
	if csr == nil {
		t.Fatal("auth.ValidateCSR returned nil csr without error")
	}
}

// caChain bundles а freshly-generated cluster CA + а leaf cert signed
// by it. Used by chain-verification tests to avoid repeating boilerplate.
type caChain struct {
	caPEM    []byte
	caCert   *x509.Certificate
	caKey    crypto.Signer
	leafPEM  []byte
	pinned   string
	leafCert *x509.Certificate
}

func newCAChain(t *testing.T, nodeName string) *caChain {
	t.Helper()
	caResult, err := auth.GenerateClusterCA(time.Now())
	if err != nil {
		t.Fatalf("auth.GenerateClusterCA: %v", err)
	}
	caCert, caDER, err := auth.ParseClusterCACert(caResult.CertPEM)
	if err != nil {
		t.Fatalf("auth.ParseClusterCACert: %v", err)
	}
	caKeyRaw, err := auth.ParseClusterCAKey(caResult.KeyPEM)
	if err != nil {
		t.Fatalf("auth.ParseClusterCAKey: %v", err)
	}
	caSigner, ok := caKeyRaw.(crypto.Signer)
	if !ok {
		t.Fatalf("CA key %T does not implement crypto.Signer", caKeyRaw)
	}

	leafPriv, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	csrTmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: "node-" + nodeName}}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, csrTmpl, leafPriv)
	if err != nil {
		t.Fatalf("CreateCertificateRequest: %v", err)
	}
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		t.Fatalf("ParseCertificateRequest: %v", err)
	}
	leafPEM, leafCert, err := auth.SignCSR(csr, nodeName, caCert, caSigner, time.Now())
	if err != nil {
		t.Fatalf("auth.SignCSR: %v", err)
	}

	pinned := hex.EncodeToString(sha256Sum(caDER))
	return &caChain{
		caPEM:    caResult.CertPEM,
		caCert:   caCert,
		caKey:    caSigner,
		leafPEM:  leafPEM,
		leafCert: leafCert,
		pinned:   pinned,
	}
}

func sha256Sum(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

func TestVerifyResponseChain(t *testing.T) {
	chain := newCAChain(t, "test-node")

	t.Run("happy path", func(t *testing.T) {
		if err := verifyResponseChain(string(chain.leafPEM), string(chain.caPEM), chain.pinned); err != nil {
			t.Errorf("verifyResponseChain: %v", err)
		}
	})

	t.Run("fingerprint mismatch surfaces sentinel", func(t *testing.T) {
		wrong := strings.Repeat("0", 64)
		err := verifyResponseChain(string(chain.leafPEM), string(chain.caPEM), wrong)
		var fpErr *FingerprintMismatchError
		if !errors.As(err, &fpErr) {
			t.Fatalf("expected *FingerprintMismatchError, got %T: %v", err, err)
		}
		if fpErr.Expected != wrong {
			t.Errorf("FingerprintMismatchError.Expected = %q, want %q", fpErr.Expected, wrong)
		}
		if fpErr.Computed != chain.pinned {
			t.Errorf("FingerprintMismatchError.Computed = %q, want %q", fpErr.Computed, chain.pinned)
		}
	})

	t.Run("malformed leaf PEM", func(t *testing.T) {
		if err := verifyResponseChain("not-a-pem", string(chain.caPEM), chain.pinned); err == nil {
			t.Error("expected error on malformed leaf PEM, got nil")
		}
	})

	t.Run("malformed CA PEM", func(t *testing.T) {
		if err := verifyResponseChain(string(chain.leafPEM), "not-a-pem", chain.pinned); err == nil {
			t.Error("expected error on malformed CA PEM, got nil")
		}
	})

	t.Run("leaf not chained to CA", func(t *testing.T) {
		other := newCAChain(t, "other-node")
		// Combine: chain.caPEM (pinned) but leaf signed by other CA.
		err := verifyResponseChain(string(other.leafPEM), string(chain.caPEM), chain.pinned)
		if err == nil {
			t.Error("expected chain verification error, got nil")
		}
		var fpErr *FingerprintMismatchError
		if errors.As(err, &fpErr) {
			t.Errorf("expected non-fingerprint error, got %v", err)
		}
	})
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")

	if err := writeFileAtomic(path, []byte("hello"), 0o600); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("content = %q, want %q", got, "hello")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0o600", info.Mode().Perm())
	}

	if err := writeFileAtomic(path, []byte("world"), 0o644); err != nil {
		t.Fatalf("writeFileAtomic overwrite: %v", err)
	}
	got, _ = os.ReadFile(path)
	if string(got) != "world" {
		t.Errorf("after overwrite content = %q, want %q", got, "world")
	}
}

func TestWriteFileAtomic_CreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "nested", "deep", "file.txt")
	if err := writeFileAtomic(nested, []byte("x"), 0o644); err != nil {
		t.Fatalf("writeFileAtomic on nested path: %v", err)
	}
	if _, err := os.Stat(nested); err != nil {
		t.Errorf("nested file not present: %v", err)
	}
}

// TestPersist verifies the three-file atomic write contract for the
// name-keyed identity layout (no node-id sidecar).
func TestPersist(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.crt")
	keyPath := filepath.Join(dir, "key.pem")
	caPath := filepath.Join(dir, "ca.crt")

	result := &Result{
		NodeID:    "11111111-1111-1111-1111-111111111111",
		KeyPEM:    []byte("KEYBYTES"),
		CertPEM:   []byte("CERTBYTES"),
		CACertPEM: []byte("CABYTES"),
	}

	if err := Persist(certPath, keyPath, caPath, result); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	type want struct {
		path string
		body string
		mode os.FileMode
	}
	cases := []want{
		{keyPath, "KEYBYTES", 0o600},
		{certPath, "CERTBYTES", 0o644},
		{caPath, "CABYTES", 0o644},
	}
	for _, c := range cases {
		body, err := os.ReadFile(c.path)
		if err != nil {
			t.Errorf("ReadFile %s: %v", c.path, err)
			continue
		}
		if string(body) != c.body {
			t.Errorf("%s body = %q, want %q", c.path, body, c.body)
		}
		info, err := os.Stat(c.path)
		if err != nil {
			t.Errorf("Stat %s: %v", c.path, err)
			continue
		}
		if info.Mode().Perm() != c.mode {
			t.Errorf("%s mode = %v, want %v", c.path, info.Mode().Perm(), c.mode)
		}
	}

	// Sidecar removed per L9 — verify it is NOT written.
	if _, err := os.Stat(filepath.Join(dir, "node-id")); !os.IsNotExist(err) {
		t.Errorf("node-id sidecar should not exist after Persist; stat err = %v", err)
	}
}

func TestPersist_NilResult(t *testing.T) {
	dir := t.TempDir()
	err := Persist(
		filepath.Join(dir, "c.crt"),
		filepath.Join(dir, "k.pem"),
		filepath.Join(dir, "ca.crt"),
		nil,
	)
	if err == nil {
		t.Error("Persist(nil) = nil err, want error")
	}
}

func TestResolveToken_Literal(t *testing.T) {
	cfg := &config.BootstrapConfig{Token: "otx_join_abc"}
	got, err := resolveToken(cfg)
	if err != nil {
		t.Fatalf("resolveToken: %v", err)
	}
	if got != "otx_join_abc" {
		t.Errorf("got %q, want %q", got, "otx_join_abc")
	}
}

func TestResolveToken_File(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("otx_join_xyz\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg := &config.BootstrapConfig{TokenPath: path}
	got, err := resolveToken(cfg)
	if err != nil {
		t.Fatalf("resolveToken: %v", err)
	}
	if got != "otx_join_xyz" {
		t.Errorf("got %q, want trimmed %q", got, "otx_join_xyz")
	}
}

func TestResolveToken_FileMissing(t *testing.T) {
	cfg := &config.BootstrapConfig{TokenPath: "/nonexistent-bootstrap-token-path"}
	if _, err := resolveToken(cfg); err == nil {
		t.Error("resolveToken on missing path = nil err, want error")
	}
}

func TestResolveToken_FileEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty")
	if err := os.WriteFile(path, []byte("   \n   "), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg := &config.BootstrapConfig{TokenPath: path}
	if _, err := resolveToken(cfg); err == nil {
		t.Error("resolveToken on whitespace-only file = nil err, want error")
	}
}

func TestResolveToken_NeitherSet(t *testing.T) {
	cfg := &config.BootstrapConfig{}
	if _, err := resolveToken(cfg); err == nil {
		t.Error("resolveToken with no token и no path = nil err, want error")
	}
}

func TestTokenHashPrefix(t *testing.T) {
	if got := tokenHashPrefix(""); got != "(empty)" {
		t.Errorf("tokenHashPrefix(\"\") = %q, want %q", got, "(empty)")
	}
	sum := sha256.Sum256([]byte("otx_join_abc"))
	want := hex.EncodeToString(sum[:])[:8]
	if got := tokenHashPrefix("otx_join_abc"); got != want {
		t.Errorf("tokenHashPrefix = %q, want %q", got, want)
	}
}
