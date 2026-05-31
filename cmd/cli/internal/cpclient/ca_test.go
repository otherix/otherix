// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cpclient_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

func TestGetCA_HappyPath(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ca" {
			t.Errorf("path = %s, want /v1/ca", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"cas": []map[string]any{{
				"cert_pem":           "-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n",
				"fingerprint_sha256": strings.Repeat("a", 64),
				"not_before":         "2026-05-13T12:00:00Z",
				"not_after":          "2036-05-13T12:00:00Z",
			}},
			"signer_fingerprint_sha256": strings.Repeat("a", 64),
		})
	}))
	defer srv.Close()

	c, err := cpclient.NewAnonymous(srv.URL, cpclient.Options{})
	if err != nil {
		t.Fatalf("NewAnonymous: %v", err)
	}
	out, err := c.GetCA(context.Background())
	if err != nil {
		t.Fatalf("GetCA: %v", err)
	}
	if len(out.CAs) != 1 {
		t.Fatalf("CAs = %d, want 1", len(out.CAs))
	}
	if !strings.HasPrefix(out.CAs[0].CertPEM, "-----BEGIN CERTIFICATE-----") {
		t.Errorf("CAs[0].CertPEM missing PEM header: %q", out.CAs[0].CertPEM)
	}
	if out.SignerFingerprintSHA256 != strings.Repeat("a", 64) {
		t.Errorf("SignerFingerprintSHA256 = %q, want 64 'a' chars", out.SignerFingerprintSHA256)
	}
}

// TestGetCA_AnonymousNoBearerHeader verifies the anonymous-client path
// does not emit an Authorization header.
func TestGetCA_AnonymousNoBearerHeader(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("anonymous Client emitted Authorization = %q, must be empty", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"cas": []map[string]any{{
				"cert_pem":           "-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n",
				"fingerprint_sha256": strings.Repeat("0", 64),
				"not_before":         "2026-01-01T00:00:00Z",
				"not_after":          "2036-01-01T00:00:00Z",
			}},
			"signer_fingerprint_sha256": strings.Repeat("0", 64),
		})
	}))
	defer srv.Close()

	c, err := cpclient.NewAnonymous(srv.URL, cpclient.Options{})
	if err != nil {
		t.Fatalf("NewAnonymous: %v", err)
	}
	if _, err := c.GetCA(context.Background()); err != nil {
		t.Errorf("GetCA: %v", err)
	}
}
