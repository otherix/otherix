// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"regexp"
	"strings"
	"testing"
)

var fingerprintHex = regexp.MustCompile(`^[a-f0-9]{64}$`)

// TestClusterCABundle covers the anonymous /v1/ca endpoint returning the trust
// set as a bundle. With no rotation the bundle holds exactly one CA, which is
// also the signer.
func TestClusterCABundle(t *testing.T) {
	h := newE2E(t)

	resp := h.get(t, "/v1/ca", "")
	if resp.StatusCode != 200 {
		t.Fatalf("/v1/ca status = %d, want 200", resp.StatusCode)
	}

	var body struct {
		CAs []struct {
			CertPEM           string `json:"cert_pem"`
			FingerprintSHA256 string `json:"fingerprint_sha256"`
			NotBefore         string `json:"not_before"`
			NotAfter          string `json:"not_after"`
		} `json:"cas"`
		SignerFingerprintSHA256 string `json:"signer_fingerprint_sha256"`
	}
	decodeJSON(t, resp, &body)

	if len(body.CAs) != 1 {
		t.Fatalf("/v1/ca cas = %d, want 1 (single CA, no rotation)", len(body.CAs))
	}
	ca := body.CAs[0]
	if !strings.Contains(ca.CertPEM, "BEGIN CERTIFICATE") {
		t.Errorf("cas[0].cert_pem is not PEM: %q", ca.CertPEM)
	}
	if !fingerprintHex.MatchString(ca.FingerprintSHA256) {
		t.Errorf("cas[0].fingerprint_sha256 = %q, want 64 lowercase hex", ca.FingerprintSHA256)
	}
	if ca.NotBefore == "" || ca.NotAfter == "" {
		t.Errorf("cas[0] validity window missing: not_before=%q not_after=%q", ca.NotBefore, ca.NotAfter)
	}
	if body.SignerFingerprintSHA256 != ca.FingerprintSHA256 {
		t.Errorf("signer_fingerprint_sha256 = %q, want the sole CA fingerprint %q",
			body.SignerFingerprintSHA256, ca.FingerprintSHA256)
	}
}
