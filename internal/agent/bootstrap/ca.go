// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package bootstrap

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/otherix/otherix/internal/auth"
)

// fingerprintHexPattern matches the canonical 64-char lowercase hex
// form (no prefix, lowercase only). Mirrors the BootstrapConfig.Validate
// pattern in internal/config — duplicated rather than imported because
// config holds the validation contract and bootstrap holds the runtime
// normaliser, and crossing the package boundary for a regexp adds an
// unhelpful coupling.
var fingerprintHexPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// NormalizeFingerprint canonicalises a fingerprint string and returns
// the 64-char lowercase hex form. Accepts:
//
//   - "sha256:<hex>" (CLI emit form per join_token_create.go).
//   - "<hex>" with either case (env var paste, programmatic input).
//
// Surrounding whitespace is trimmed. Any other shape returns an
// error mentioning the original input — operators can spot typos
// quickly.
func NormalizeFingerprint(input string) (string, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return "", errors.New("fingerprint is empty")
	}
	s = strings.TrimPrefix(s, "sha256:")
	s = strings.ToLower(s)
	if !fingerprintHexPattern.MatchString(s) {
		return "", fmt.Errorf("fingerprint must be 64 lowercase hex chars (optional sha256: prefix), got %q", input)
	}
	return s, nil
}

// caResponse mirrors components/schemas/ClusterCA in control-plane.yaml.
// Hand-written because agent-side oapi-codegen targets agent.yaml only —
// the CP API contract is consumed by the agent, not served by it.
type caResponse struct {
	CertPEM           string `json:"cert_pem"`
	FingerprintSHA256 string `json:"fingerprint_sha256"`
	NotBefore         string `json:"not_before"`
	NotAfter          string `json:"not_after"`
}

// fetchAndVerifyCA performs the first bootstrap network round-trip:
// GET /v1/ca with InsecureSkipVerify, then sha256(cert.Raw) compared to
// the operator-pinned fingerprint. The TLS skip is essential because
// the CP's serving cert may not chain to the cluster CA we are
// bootstrapping against.
//
// Returns the cert PEM bytes + parsed x509 cert. The parsed cert is
// useful to the caller for chain verification (see verifyResponseChain
// in csr.go) and for slog labelling.
func fetchAndVerifyCA(ctx context.Context, cpURL, expectedFingerprint string, timeout time.Duration) ([]byte, *x509.Certificate, error) {
	client := &http.Client{
		Timeout:   timeout,
		Transport: newBootstrapTransport(),
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, cpURL+"/v1/ca", nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build /v1/ca request: %v", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch /v1/ca: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, nil, fmt.Errorf("fetch /v1/ca: HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(preview)))
	}

	var body caResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, nil, fmt.Errorf("decode /v1/ca response: %v", err)
	}
	if body.CertPEM == "" {
		return nil, nil, errors.New("/v1/ca response has empty cert_pem")
	}

	cert, _, err := auth.ParseClusterCACert([]byte(body.CertPEM))
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA cert from /v1/ca: %v", err)
	}

	computed := sha256.Sum256(cert.Raw)
	computedHex := hex.EncodeToString(computed[:])
	if computedHex != expectedFingerprint {
		return nil, nil, &FingerprintMismatchError{
			Expected: expectedFingerprint,
			Computed: computedHex,
		}
	}

	// Defense-in-depth: the server-reported fingerprint should also match
	// what we just computed. A mismatch here means the CP is returning
	// inconsistent data — either a bug in the CP or, more likely, a
	// MITM rewriting only parts of the JSON response.
	serverReported := strings.ToLower(strings.TrimSpace(body.FingerprintSHA256))
	if serverReported != computedHex {
		return nil, nil, fmt.Errorf(
			"server-reported fingerprint %q does not match computed %q from cert_pem",
			serverReported, computedHex,
		)
	}

	return []byte(body.CertPEM), cert, nil
}

// newBootstrapTransport is the *http.Transport used for every
// bootstrap request. Captures the policy decisions:
//
//   - InsecureSkipVerify: true — TOFU, fingerprint match compensates.
//   - MinVersion: TLS 1.2 — modern floor; TLS 1.3 preferred but 1.2
//     accepted because the CP may be behind older ingress.
//   - Proxy: nil — env-var proxies (HTTP_PROXY etc.) intentionally
//     ignored. Operators running bootstrap behind a corporate proxy
//     must surface that complexity explicitly in a future iteration.
func newBootstrapTransport() *http.Transport {
	return &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // TOFU - security from post-receipt fingerprint + chain verification
			MinVersion:         tls.VersionTLS12,
		},
		Proxy:                 nil,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}
}
