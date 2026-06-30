// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package bootstrap

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	keyPEM, csrPEM, priv, err := generateKeypairAndCSR(nodeCNPrefix, "test-node")
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
	_, csrPEM, _, err := generateKeypairAndCSR(nodeCNPrefix, "validate-target")
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

// TestGenerateKeypairAndCSR_GatewayPrefix confirms the gateway identity prefix
// threads into both the Subject CN and the SAN DNS, so a gateway bootstrap mints
// a "gateway-<name>" CSR (matching the cert the CP issues via SignGatewayCSR).
func TestGenerateKeypairAndCSR_GatewayPrefix(t *testing.T) {
	_, csrPEM, _, err := generateKeypairAndCSR(gatewayCNPrefix, "edge1")
	if err != nil {
		t.Fatalf("generateKeypairAndCSR: %v", err)
	}
	block, _ := pem.Decode(csrPEM)
	if block == nil {
		t.Fatal("decode csrPEM: nil block")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatalf("parse csr: %v", err)
	}
	if got, want := csr.Subject.CommonName, "gateway-edge1"; got != want {
		t.Errorf("Subject.CommonName = %q, want %q", got, want)
	}
	wantDNS := "gateway-edge1.agents.otherix.local"
	found := false
	for _, dns := range csr.DNSNames {
		if dns == wantDNS {
			found = true
		}
	}
	if !found {
		t.Errorf("CSR DNSNames missing %q (have %v)", wantDNS, csr.DNSNames)
	}
}

// caChain bundles a freshly-generated cluster CA + a leaf cert signed
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
	leafPEM, leafCert, err := auth.SignCSR(csr, nodeName, "", caCert, caSigner, time.Now())
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

func TestFetchAndVerifyCA_Bundle(t *testing.T) {
	chain := newCAChain(t, "bundle-node")

	bundle := map[string]any{
		"cas": []map[string]any{{
			"cert_pem":           string(chain.caPEM),
			"fingerprint_sha256": chain.pinned,
			"not_before":         time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
			"not_after":          time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		}},
		"signer_fingerprint_sha256": chain.pinned,
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ca" {
			t.Errorf("path = %s, want /v1/ca", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(bundle)
	}))
	defer srv.Close()

	t.Run("pinned CA present in bundle", func(t *testing.T) {
		bundlePEM, cert, err := fetchAndVerifyCA(context.Background(), srv.URL, chain.pinned, 5*time.Second)
		if err != nil {
			t.Fatalf("fetchAndVerifyCA: %v", err)
		}
		if !strings.Contains(string(bundlePEM), "BEGIN CERTIFICATE") {
			t.Errorf("bundlePEM is not PEM: %q", bundlePEM)
		}
		if cert == nil {
			t.Fatal("pinned cert is nil")
		}
		got := hex.EncodeToString(sha256Sum(cert.Raw))
		if got != chain.pinned {
			t.Errorf("pinned cert fingerprint = %q, want %q", got, chain.pinned)
		}
	})

	t.Run("pinned fingerprint absent from bundle", func(t *testing.T) {
		if _, _, err := fetchAndVerifyCA(context.Background(), srv.URL, strings.Repeat("b", 64), 5*time.Second); err == nil {
			t.Fatal("expected error when pinned fingerprint not in bundle, got nil")
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
		t.Error("resolveToken with no token and no path = nil err, want error")
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

// TestNewBootstrapTransportPinsTLS13Floor asserts the TOFU /v1/ca
// fetch transport refuses TLS handshakes below 1.3. The CP listener is
// Go crypto/tls (1.3 always available), so the bootstrap dialer has no
// compatibility reason to keep a 1.2 floor.
func TestNewBootstrapTransportPinsTLS13Floor(t *testing.T) {
	tr := newBootstrapTransport()
	if got := tr.TLSClientConfig.MinVersion; got != tls.VersionTLS13 {
		t.Errorf("TLSClientConfig.MinVersion = %#x, want %#x (TLS 1.3)", got, uint16(tls.VersionTLS13))
	}
}

func TestVerifyingTransportPinsCAAndTLS13(t *testing.T) {
	caResult, err := auth.GenerateClusterCA(time.Now())
	if err != nil {
		t.Fatalf("GenerateClusterCA: %v", err)
	}
	caCert, _, err := auth.ParseClusterCACert(caResult.CertPEM)
	if err != nil {
		t.Fatalf("ParseClusterCACert: %v", err)
	}
	tr, err := verifyingTransport(caCert)
	if err != nil {
		t.Fatalf("verifyingTransport: %v", err)
	}
	if tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("verifyingTransport must NOT skip verification")
	}
	if tr.TLSClientConfig.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %d, want TLS 1.3 (%d)", tr.TLSClientConfig.MinVersion, tls.VersionTLS13)
	}
	if tr.TLSClientConfig.RootCAs == nil {
		t.Error("RootCAs must be set to the pinned CA")
	}
}

func TestVerifyingTransportRejectsNilAnchor(t *testing.T) {
	if _, err := verifyingTransport(nil); err == nil {
		t.Error("verifyingTransport(nil) = nil error, want failure")
	}
}

// startJoinTLSServer starts an httptest TLS server presenting a leaf signed by
// signCA (CN otherix-cp-replica, IP SAN 127.0.0.1 so the 127.0.0.1 dial verifies).
// It records whether the /v1/nodes/join handler was reached. Returns the server
// and the reached flag (atomic - the handler runs on the server goroutine).
func startJoinTLSServer(t *testing.T, signCA auth.ClusterCAResult) (*httptest.Server, *atomic.Bool) {
	t.Helper()
	caCert, _, err := auth.ParseClusterCACert(signCA.CertPEM)
	if err != nil {
		t.Fatalf("ParseClusterCACert: %v", err)
	}
	caKeyAny, err := auth.ParseClusterCAKey(signCA.KeyPEM)
	if err != nil {
		t.Fatalf("ParseClusterCAKey: %v", err)
	}
	caSigner, ok := caKeyAny.(crypto.Signer)
	if !ok {
		t.Fatalf("CA key %T does not implement crypto.Signer", caKeyAny)
	}
	leafCertPEM, leafKeyPEM, err := auth.GenerateReplicaCert(caCert, caSigner, nil, []net.IP{net.ParseIP("127.0.0.1")}, time.Hour, time.Now())
	if err != nil {
		t.Fatalf("GenerateReplicaCert: %v", err)
	}
	leaf, err := tls.X509KeyPair(leafCertPEM, leafKeyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}

	var reached atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/nodes/join", func(w http.ResponseWriter, r *http.Request) {
		reached.Store(true)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(joinResponse{NodeID: "n1", CertPEM: "x", CACertPEM: "y"})
	})
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, &reached
}

func TestSubmitCSR_VerifiedServerReached(t *testing.T) {
	ca, err := auth.GenerateClusterCA(time.Now())
	if err != nil {
		t.Fatalf("GenerateClusterCA: %v", err)
	}
	caCert, _, err := auth.ParseClusterCACert(ca.CertPEM)
	if err != nil {
		t.Fatalf("ParseClusterCACert: %v", err)
	}
	srv, reached := startJoinTLSServer(t, ca)
	cfg := &config.BootstrapConfig{CPURL: srv.URL, NodeName: "node-1", Architecture: "arm64"}

	_, err = submitCSR(context.Background(), cfg, "tok", "csr", caCert, 10*time.Second)
	if err != nil {
		t.Fatalf("submitCSR against verified server: %v", err)
	}
	if !reached.Load() {
		t.Error("join handler not reached over a verified connection")
	}
}

// startBootstrapTLSServer starts an httptest TLS server serving BOTH /v1/ca
// and /v1/nodes/join. /v1/ca returns every advertisedCAs entry, each with a
// self-consistent fingerprint (the first entry is the bundle signer); the
// server's TLS cert is signed by tlsSignCA (equal to the pinned CA in the
// positive case, a different CA in the MITM cases). It records whether the
// join handler was reached (atomic - the handler runs on the server
// goroutine).
func startBootstrapTLSServer(t *testing.T, tlsSignCA auth.ClusterCAResult, advertisedCAs ...auth.ClusterCAResult) (*httptest.Server, *atomic.Bool) {
	t.Helper()
	caCert, _, err := auth.ParseClusterCACert(tlsSignCA.CertPEM)
	if err != nil {
		t.Fatalf("parse tls ca: %v", err)
	}
	caKeyAny, err := auth.ParseClusterCAKey(tlsSignCA.KeyPEM)
	if err != nil {
		t.Fatalf("parse tls ca key: %v", err)
	}
	caSigner, ok := caKeyAny.(crypto.Signer)
	if !ok {
		t.Fatalf("CA key %T does not implement crypto.Signer", caKeyAny)
	}
	leafCertPEM, leafKeyPEM, err := auth.GenerateReplicaCert(caCert, caSigner, nil, []net.IP{net.ParseIP("127.0.0.1")}, time.Hour, time.Now())
	if err != nil {
		t.Fatalf("gen leaf: %v", err)
	}
	leaf, err := tls.X509KeyPair(leafCertPEM, leafKeyPEM)
	if err != nil {
		t.Fatalf("x509 keypair: %v", err)
	}

	if len(advertisedCAs) == 0 {
		t.Fatal("startBootstrapTLSServer needs at least one advertised CA")
	}
	entries := make([]caCertEntry, 0, len(advertisedCAs))
	for _, adv := range advertisedCAs {
		advCert, _, err := auth.ParseClusterCACert(adv.CertPEM)
		if err != nil {
			t.Fatalf("parse adv ca: %v", err)
		}
		advFP := sha256.Sum256(advCert.Raw)
		entries = append(entries, caCertEntry{
			CertPEM:           string(adv.CertPEM),
			FingerprintSHA256: hex.EncodeToString(advFP[:]),
		})
	}

	var reached atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/ca", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(caBundleResponse{
			CAs:                     entries,
			SignerFingerprintSHA256: entries[0].FingerprintSHA256,
		})
	})
	mux.HandleFunc("/v1/nodes/join", func(w http.ResponseWriter, r *http.Request) {
		reached.Store(true)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(joinResponse{NodeID: "n1", CertPEM: "x", CACertPEM: "y"})
	})
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, &reached
}

// TestBootstrap_TokenOnlySentOverVerifiedTLS seam-proves through the REAL
// Bootstrap entry point that the join token is transmitted only after the CP
// serving cert verifies against the fingerprint-pinned cluster CA.
func TestBootstrap_TokenOnlySentOverVerifiedTLS(t *testing.T) {
	ca, err := auth.GenerateClusterCA(time.Now())
	if err != nil {
		t.Fatalf("gen ca: %v", err)
	}
	advCert, _, err := auth.ParseClusterCACert(ca.CertPEM)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}
	advSum := sha256.Sum256(advCert.Raw)
	advFP := hex.EncodeToString(advSum[:])

	t.Run("verified server reaches join", func(t *testing.T) {
		srv, reached := startBootstrapTLSServer(t, ca, ca) // tls cert == advertised CA
		cfg := &config.BootstrapConfig{Token: "tok", CPURL: srv.URL, CAFingerprint: advFP, NodeName: "node-1", Architecture: "arm64"}
		_, _ = Bootstrap(context.Background(), cfg, nil) // may fail later at verifyResponseChain; we assert reach
		if !reached.Load() {
			t.Error("join handler not reached against a verified server")
		}
	})

	t.Run("mitm tls cert aborts before join", func(t *testing.T) {
		// Backdate the MITM CA so the handshake fails on UNTRUSTED chain, not on
		// not-yet-valid validity - otherwise a pool-widening regression could slip
		// past this test on timing alone.
		mitm, err := auth.GenerateClusterCA(time.Now().Add(-time.Hour))
		if err != nil {
			t.Fatalf("gen mitm ca: %v", err)
		}
		srv, reached := startBootstrapTLSServer(t, mitm, ca) // /v1/ca advertises real ca, TLS cert from mitm
		cfg := &config.BootstrapConfig{Token: "tok", CPURL: srv.URL, CAFingerprint: advFP, NodeName: "node-1", Architecture: "arm64"}
		_, err = Bootstrap(context.Background(), cfg, nil)
		if err == nil {
			t.Fatal("Bootstrap against MITM tls cert = nil error, want failure")
		}
		if reached.Load() {
			t.Error("join handler reached over UNVERIFIED TLS - token leaked to MITM")
		}
	})
}

// TestBootstrap_SecondBundleCACannotAuthenticateJoin seam-proves the join
// POST verifies against ONLY the operator-pinned CA, not the whole TOFU
// bundle. Attack encoded: an active MITM on the unauthenticated /v1/ca fetch
// returns [realPinnedCA, attackerCA] - both entries self-consistent (correct
// per-entry fingerprints) and the operator pin present - then presents an
// attackerCA-signed leaf to /v1/nodes/join. If any bundle entry beyond the
// pinned cert lands in the POST trust pool, the handshake verifies and the
// token leaks. The handshake must fail and the join handler must never run.
func TestBootstrap_SecondBundleCACannotAuthenticateJoin(t *testing.T) {
	realCA, err := auth.GenerateClusterCA(time.Now())
	if err != nil {
		t.Fatalf("gen real ca: %v", err)
	}
	// Backdate the attacker CA so it is valid NOW - the test must fail on
	// pool narrowing, not on a not-yet-valid attacker cert.
	attackerCA, err := auth.GenerateClusterCA(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("gen attacker ca: %v", err)
	}
	realCert, _, err := auth.ParseClusterCACert(realCA.CertPEM)
	if err != nil {
		t.Fatalf("parse real ca: %v", err)
	}
	realSum := sha256.Sum256(realCert.Raw)

	// TLS leaf signed by attackerCA; /v1/ca bundle carries BOTH CAs.
	srv, reached := startBootstrapTLSServer(t, attackerCA, realCA, attackerCA)
	cfg := &config.BootstrapConfig{
		Token:         "tok",
		CPURL:         srv.URL,
		CAFingerprint: hex.EncodeToString(realSum[:]),
		NodeName:      "node-1",
		Architecture:  "arm64",
	}

	_, err = Bootstrap(context.Background(), cfg, nil)
	if err == nil {
		t.Fatal("Bootstrap with attacker CA smuggled into TOFU bundle = nil error, want TLS verification failure")
	}
	if reached.Load() {
		t.Error("join handler reached via attacker bundle CA - token leaked despite operator pin")
	}
}

func TestSubmitCSR_MITMServerRejectedTokenNotSent(t *testing.T) {
	// Pin the REAL cluster CA, but the server's TLS cert is signed by a DIFFERENT
	// CA (the MITM): verification must fail and the handler must never be reached.
	realCA, err := auth.GenerateClusterCA(time.Now())
	if err != nil {
		t.Fatalf("GenerateClusterCA real: %v", err)
	}
	// Backdate so the handshake fails on UNTRUSTED chain, not not-yet-valid.
	mitmCA, err := auth.GenerateClusterCA(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("GenerateClusterCA mitm: %v", err)
	}
	realCert, _, err := auth.ParseClusterCACert(realCA.CertPEM)
	if err != nil {
		t.Fatalf("ParseClusterCACert real: %v", err)
	}
	srv, reached := startJoinTLSServer(t, mitmCA) // server cert chains to mitmCA
	cfg := &config.BootstrapConfig{CPURL: srv.URL, NodeName: "node-1", Architecture: "arm64"}

	_, err = submitCSR(context.Background(), cfg, "tok", "csr", realCert, 10*time.Second)
	if err == nil {
		t.Fatal("submitCSR against MITM server = nil error, want TLS verification failure")
	}
	if reached.Load() {
		t.Error("join handler was reached over an UNVERIFIED connection - token leaked")
	}
}
