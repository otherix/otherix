// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package catrust fetches and pins a control-plane cluster CA over a
// trust-on-first-use connection, so the operator CLI can store the CA
// inline in its config. It mirrors the server-side joiner bootstrap:
// connect with verification disabled, read GET /v1/ca, then pin by an
// operator-supplied fingerprint or an interactive confirmation before
// trusting the returned PEM bundle.
package catrust

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// fetchTimeout bounds the single TOFU round-trip.
const fetchTimeout = 15 * time.Second

type caCertEntry struct {
	CertPEM           string `json:"cert_pem"`
	FingerprintSHA256 string `json:"fingerprint_sha256"`
}

type caResponse struct {
	CAs                     []caCertEntry `json:"cas"`
	SignerFingerprintSHA256 string        `json:"signer_fingerprint_sha256"`
}

// FetchAndPin connects to serverURL with TLS verification disabled,
// fetches /v1/ca, and decides whether to trust the returned CA bundle:
//
//   - wantFingerprint != "" : the hex sha256 must equal one of the
//     returned CAs' fingerprints, else an error (possible MITM).
//   - wantFingerprint == ""  : confirm is called with the signer
//     fingerprint; trusting proceeds only if it returns (true, nil).
//
// On trust it returns the concatenated PEM bundle of every CA in the
// response. confirm may be nil only when wantFingerprint is set.
func FetchAndPin(ctx context.Context, serverURL, wantFingerprint string, confirm func(fingerprint string) (bool, error)) ([]byte, error) {
	resp, err := fetchCA(ctx, serverURL)
	if err != nil {
		return nil, err
	}
	if len(resp.CAs) == 0 {
		return nil, errors.New("catrust: /v1/ca returned an empty cas bundle")
	}

	bundle, fingerprints, err := verifyBundle(resp)
	if err != nil {
		return nil, err
	}

	want := strings.ToLower(strings.TrimSpace(wantFingerprint))
	if want != "" {
		for _, fp := range fingerprints {
			if fp == want {
				return bundle, nil
			}
		}
		return nil, fmt.Errorf("catrust: pinned fingerprint %q not present in /v1/ca bundle - refusing to trust (possible MITM)", want)
	}

	if confirm == nil {
		return nil, errors.New("catrust: no fingerprint and no confirmation callback")
	}
	signer := strings.ToLower(strings.TrimSpace(resp.SignerFingerprintSHA256))
	ok, err := confirm(signer)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("catrust: CA not trusted by operator")
	}
	return bundle, nil
}

// fetchCA performs the insecure GET /v1/ca round-trip.
func fetchCA(ctx context.Context, serverURL string) (caResponse, error) {
	client := &http.Client{
		Timeout: fetchTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // TOFU: pinned by fingerprint/confirm below
		},
	}
	url := strings.TrimRight(serverURL, "/") + "/v1/ca"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return caResponse{}, fmt.Errorf("catrust: build request: %v", err)
	}
	httpResp, err := client.Do(req)
	if err != nil {
		return caResponse{}, fmt.Errorf("catrust: fetch /v1/ca: %v", err)
	}
	defer func() { _ = httpResp.Body.Close() }()
	if httpResp.StatusCode != http.StatusOK {
		return caResponse{}, fmt.Errorf("catrust: fetch /v1/ca: HTTP %d", httpResp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
	if err != nil {
		return caResponse{}, fmt.Errorf("catrust: read /v1/ca: %v", err)
	}
	var resp caResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return caResponse{}, fmt.Errorf("catrust: decode /v1/ca: %v", err)
	}
	return resp, nil
}

// verifyBundle re-computes each CA's fingerprint from its PEM, checks it
// against the server-reported value, and returns the concatenated PEM
// bundle plus the lowercase computed fingerprints.
func verifyBundle(resp caResponse) (bundle []byte, fingerprints []string, err error) {
	for i, entry := range resp.CAs {
		if entry.CertPEM == "" {
			return nil, nil, fmt.Errorf("catrust: cas[%d] has empty cert_pem", i)
		}
		block, _ := pem.Decode([]byte(entry.CertPEM))
		if block == nil || block.Type != "CERTIFICATE" {
			return nil, nil, fmt.Errorf("catrust: cas[%d] is not a PEM certificate", i)
		}
		sum := sha256.Sum256(block.Bytes)
		computed := hex.EncodeToString(sum[:])
		reported := strings.ToLower(strings.TrimSpace(entry.FingerprintSHA256))
		if reported != "" && reported != computed {
			return nil, nil, fmt.Errorf("catrust: cas[%d] reported fingerprint %q != computed %q", i, reported, computed)
		}
		bundle = append(bundle, []byte(entry.CertPEM)...)
		if len(bundle) > 0 && bundle[len(bundle)-1] != '\n' {
			bundle = append(bundle, '\n')
		}
		fingerprints = append(fingerprints, computed)
	}
	return bundle, fingerprints, nil
}
