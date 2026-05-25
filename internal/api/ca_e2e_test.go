// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package api_test

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/auth"
)

// TestE2E_CA_GetAnonymous_HappyPath verifies anonymous access to
// /v1/ca returns a valid PEM cert plus a fingerprint that matches
// sha256(cert.Raw) computed independently from the PEM payload.
func TestE2E_CA_GetAnonymous_HappyPath(t *testing.T) {
	h := newE2E(t)
	resp := h.get(t, "/v1/ca", "") // empty bearer ⇒ anonymous
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var view struct {
		CertPEM           string `json:"cert_pem"`
		FingerprintSHA256 string `json:"fingerprint_sha256"`
		NotBefore         string `json:"not_before"`
		NotAfter          string `json:"not_after"`
	}
	decodeJSON(t, resp, &view)

	if !strings.Contains(view.CertPEM, "BEGIN CERTIFICATE") {
		t.Errorf("cert_pem does not contain BEGIN CERTIFICATE block")
	}
	cert, der, err := auth.ParseClusterCACert([]byte(view.CertPEM))
	if err != nil {
		t.Fatalf("ParseClusterCACert: %v", err)
	}
	wantFP := sha256.Sum256(der)
	if got := hex.EncodeToString(wantFP[:]); got != view.FingerprintSHA256 {
		t.Errorf("fingerprint = %s, want %s", view.FingerprintSHA256, got)
	}
	if cert.Subject.CommonName != "otherix-cluster-ca" {
		t.Errorf("CommonName = %q, want otherix-cluster-ca", cert.Subject.CommonName)
	}

	// not_before / not_after parse back as RFC3339.
	if _, err := time.Parse(time.RFC3339Nano, view.NotBefore); err != nil {
		t.Errorf("NotBefore not RFC3339Nano: %v", err)
	}
	if _, err := time.Parse(time.RFC3339Nano, view.NotAfter); err != nil {
		t.Errorf("NotAfter not RFC3339Nano: %v", err)
	}
}

// TestE2E_CA_GetWithBearer verifies the endpoint accepts authenticated
// callers too — the spec marks `security: []` (anonymous), but an
// admin Bearer must not be rejected.
func TestE2E_CA_GetWithBearer(t *testing.T) {
	h := newE2E(t)
	admin := loginAs(t, h, auth.RoleAdmin)
	resp := h.get(t, "/v1/ca", admin)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// TestE2E_CA_DoesNotExposeKey asserts the JSON response shape does
// not carry any `key_pem` field — defense-in-depth against a
// regression that surfaces the CA private key over the wire.
func TestE2E_CA_DoesNotExposeKey(t *testing.T) {
	h := newE2E(t)
	resp := h.get(t, "/v1/ca", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()
	if strings.Contains(strings.ToLower(string(body)), "key_pem") {
		t.Error("response body contains key_pem; CA private key MUST NOT leak")
	}
	if strings.Contains(strings.ToLower(string(body)), "private key") {
		t.Error("response body contains 'PRIVATE KEY' PEM marker; CA private key MUST NOT leak")
	}
}
