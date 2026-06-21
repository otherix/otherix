// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package catrust

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newSelfSignedCA returns a DER cert and its PEM encoding for a throwaway CA.
func newSelfSignedCA(t *testing.T) (der []byte, pemBytes []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err = x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	pemBytes = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return der, pemBytes
}

// caServer serves a /v1/ca response for the given cert.
func caServer(t *testing.T, der, certPEM []byte) *httptest.Server {
	t.Helper()
	fp := sha256.Sum256(der)
	hexFP := hex.EncodeToString(fp[:])
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ca" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"cas":                       []map[string]string{{"cert_pem": string(certPEM), "fingerprint_sha256": hexFP}},
			"signer_fingerprint_sha256": hexFP,
		})
	}))
}

func TestFetchAndPin_MatchingFingerprint(t *testing.T) {
	der, certPEM := newSelfSignedCA(t)
	srv := caServer(t, der, certPEM)
	defer srv.Close()
	fp := sha256.Sum256(der)
	bundle, err := FetchAndPin(context.Background(), srv.URL, hex.EncodeToString(fp[:]), nil)
	if err != nil {
		t.Fatalf("FetchAndPin: %v", err)
	}
	if len(bundle) == 0 {
		t.Fatal("empty bundle")
	}
}

func TestFetchAndPin_MismatchFingerprint(t *testing.T) {
	der, certPEM := newSelfSignedCA(t)
	srv := caServer(t, der, certPEM)
	defer srv.Close()
	_, err := FetchAndPin(context.Background(), srv.URL, "00deadbeef", nil)
	if err == nil {
		t.Fatal("err = nil, want fingerprint mismatch")
	}
}

func TestFetchAndPin_ConfirmAccepts(t *testing.T) {
	der, certPEM := newSelfSignedCA(t)
	srv := caServer(t, der, certPEM)
	defer srv.Close()
	called := false
	bundle, err := FetchAndPin(context.Background(), srv.URL, "", func(string) (bool, error) {
		called = true
		return true, nil
	})
	if err != nil {
		t.Fatalf("FetchAndPin: %v", err)
	}
	if !called {
		t.Error("confirm callback not invoked")
	}
	if len(bundle) == 0 {
		t.Error("empty bundle on accept")
	}
}

func TestFetchAndPin_ConfirmRejects(t *testing.T) {
	der, certPEM := newSelfSignedCA(t)
	srv := caServer(t, der, certPEM)
	defer srv.Close()
	_, err := FetchAndPin(context.Background(), srv.URL, "", func(string) (bool, error) {
		return false, nil
	})
	if err == nil {
		t.Fatal("err = nil, want rejection error")
	}
}
