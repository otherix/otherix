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

// caCertEntry mirrors components/schemas/ClusterCACert.
type caCertEntry struct {
	CertPEM           string `json:"cert_pem"`
	FingerprintSHA256 string `json:"fingerprint_sha256"`
	NotBefore         string `json:"not_before"`
	NotAfter          string `json:"not_after"`
}

// caBundleResponse mirrors components/schemas/ClusterCA in control-plane.yaml:
// the CA trust set plus the active signer's fingerprint. Hand-written because
// agent-side oapi-codegen targets agent.yaml only — the CP API contract is
// consumed by the agent, not served by it.
type caBundleResponse struct {
	CAs                     []caCertEntry `json:"cas"`
	SignerFingerprintSHA256 string        `json:"signer_fingerprint_sha256"`
}

// fetchAndVerifyCA performs the first bootstrap network round-trip:
// GET /v1/ca with InsecureSkipVerify, decodes the CA trust-set bundle, and
// pins the operator fingerprint against one of its entries (TOFU). The TLS
// skip is acceptable only here: the cluster CA is not yet pinned at this
// first call, so there is nothing to verify the CP serving cert against;
// the payload is public (CA certs only); and the operator fingerprint pin
// makes any MITM substitution detectable. The subsequent token-bearing
// POST /v1/nodes/join IS verified against the CA pinned here (audit H4).
//
// Returns the concatenated trust bundle PEM (every CA the agent must trust,
// to persist as its trust anchor) plus the parsed pinned CA cert (the one
// matching the operator fingerprint, used for chain verification and slog
// labelling). Each entry's server-reported fingerprint is recomputed and
// checked (defense-in-depth against a MITM rewriting only part of the JSON).
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

	var body caBundleResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, nil, fmt.Errorf("decode /v1/ca response: %v", err)
	}
	if len(body.CAs) == 0 {
		return nil, nil, errors.New("/v1/ca response has empty cas bundle")
	}

	var bundle strings.Builder
	var pinned *x509.Certificate
	for i, entry := range body.CAs {
		if entry.CertPEM == "" {
			return nil, nil, fmt.Errorf("/v1/ca cas[%d] has empty cert_pem", i)
		}
		cert, _, err := auth.ParseClusterCACert([]byte(entry.CertPEM))
		if err != nil {
			return nil, nil, fmt.Errorf("parse /v1/ca cas[%d]: %v", i, err)
		}
		computed := sha256.Sum256(cert.Raw)
		computedHex := hex.EncodeToString(computed[:])
		// Defense-in-depth: the server-reported per-entry fingerprint must
		// match what we recompute. A mismatch means the CP is returning
		// inconsistent data or a MITM rewrote part of the JSON response.
		reported := strings.ToLower(strings.TrimSpace(entry.FingerprintSHA256))
		if reported != computedHex {
			return nil, nil, fmt.Errorf(
				"server-reported fingerprint %q for cas[%d] does not match computed %q",
				reported, i, computedHex)
		}
		if computedHex == expectedFingerprint {
			pinned = cert
		}
		bundle.WriteString(strings.TrimSpace(entry.CertPEM))
		bundle.WriteByte('\n')
	}
	if pinned == nil {
		return nil, nil, fmt.Errorf(
			"operator-pinned fingerprint %s is not present in the /v1/ca bundle (%d CAs)",
			expectedFingerprint, len(body.CAs))
	}

	return []byte(bundle.String()), pinned, nil
}

// newBootstrapTransport is the *http.Transport used for every
// bootstrap request. Captures the policy decisions:
//
//   - InsecureSkipVerify: true — TOFU, fingerprint match compensates.
//   - MinVersion: TLS 1.3 - the CP listener is Go crypto/tls (1.3
//     always available), so nothing below 1.3 is accepted.
//   - Proxy: nil — env-var proxies (HTTP_PROXY etc.) intentionally
//     ignored. Operators running bootstrap behind a corporate proxy
//     must surface that complexity explicitly in a future iteration.
func newBootstrapTransport() *http.Transport {
	return &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // TOFU - security from post-receipt fingerprint + chain verification
			MinVersion:         tls.VersionTLS13,
		},
		Proxy:                 nil,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}
}

// verifyingTransport builds the *http.Transport used for the token-bearing
// POST /v1/nodes/join. Unlike newBootstrapTransport (which the /v1/ca anchor
// fetch uses), it VERIFIES the CP serving cert against the operator-pinned
// cluster CA bundle - the token must not leave the agent until the server is
// authenticated (audit H4). The bundle is the same trust anchor steady-state
// heartbeat uses, so any CP cert that works for heartbeat verifies here.
func verifyingTransport(caBundlePEM []byte) (*http.Transport, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBundlePEM) {
		return nil, fmt.Errorf("bootstrap: pinned CA bundle contains no PEM certificates")
	}
	return &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:    pool,
			MinVersion: tls.VersionTLS13,
		},
		Proxy:                 nil,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}, nil
}
