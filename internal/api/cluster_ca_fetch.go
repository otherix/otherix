// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package api

import (
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/otherix/otherix/internal/auth"
)

// ClusterJoinFetchParams drives the joiner-side cluster CA fetch: a CP replica
// joining an existing cluster redeems its kind=cluster token at the target
// replica's /v1/cluster/join and receives the CA cert + key.
type ClusterJoinFetchParams struct {
	CPURL         string // base URL of an existing replica's control-plane API
	Token         string // plaintext kind=cluster join token
	CAFingerprint string // expected cluster CA cert fingerprint (hex sha256, optional "sha256:" prefix)
	PeerURL       string // the joiner's etcd Raft peer URL, registered as a learner by the handler
	Timeout       time.Duration
}

// ClusterMemberRef is one cluster member as returned by /v1/cluster/join, enough
// for the joiner to build its etcd initial-cluster.
type ClusterMemberRef struct {
	Name    string
	PeerURL string
}

// ClusterJoinResult is the joiner-side outcome of /v1/cluster/join: the CA to
// persist plus the current membership to seed initial-cluster.
type ClusterJoinResult struct {
	CA      auth.ClusterCAResult
	Members []ClusterMemberRef
}

// clusterJoinResponse mirrors components/schemas/ClusterJoinResponse.
type clusterJoinResponse struct {
	CACertPEM string `json:"ca_cert_pem"`
	CAKeyPEM  string `json:"ca_key_pem"`
	Members   []struct {
		Name    string `json:"name"`
		PeerURL string `json:"peer_url"`
	} `json:"members"`
}

// FetchClusterCA performs the joiner's bootstrap round-trip: first GET /v1/ca
// over a TOFU connection and pin the cluster CA by the operator fingerprint,
// then POST the cluster token to /v1/cluster/join over a transport that
// VERIFIES the target replica's serving cert against that pinned CA - the
// token never leaves the joiner before the server is authenticated.
// The returned CA is re-checked against the operator-pinned fingerprint and
// the key confirmed to pair with the cert (defense-in-depth). Returns the CA
// material to persist on disk before etcd starts.
func FetchClusterCA(ctx context.Context, p ClusterJoinFetchParams, log *slog.Logger) (ClusterJoinResult, error) {
	expected, err := normalizeCAFingerprint(p.CAFingerprint)
	if err != nil {
		return ClusterJoinResult{}, err
	}

	caPool, err := prefetchPinnedCA(ctx, p.CPURL, expected, p.Timeout)
	if err != nil {
		return ClusterJoinResult{}, err
	}

	body, err := postClusterJoin(ctx, p, caPool)
	if err != nil {
		return ClusterJoinResult{}, err
	}

	cert, der, err := auth.ParseClusterCACert([]byte(body.CACertPEM))
	if err != nil {
		return ClusterJoinResult{}, fmt.Errorf("parse cluster CA cert: %v", err)
	}
	fp := sha256.Sum256(der)
	computed := hex.EncodeToString(fp[:])
	if computed != expected {
		return ClusterJoinResult{}, fmt.Errorf(
			"cluster CA fingerprint mismatch: server returned %s, operator pinned %s", computed, expected)
	}

	if err := verifyKeyMatchesCert([]byte(body.CAKeyPEM), cert.PublicKey); err != nil {
		return ClusterJoinResult{}, err
	}

	if log != nil {
		log.InfoContext(ctx, "fetched cluster CA from existing replica",
			slog.String("cp_url", p.CPURL),
			slog.String("fingerprint_sha256", computed))
	}

	refs := make([]ClusterMemberRef, 0, len(body.Members))
	for _, m := range body.Members {
		refs = append(refs, ClusterMemberRef{Name: m.Name, PeerURL: m.PeerURL})
	}

	return ClusterJoinResult{
		CA: auth.ClusterCAResult{
			CertPEM:     []byte(body.CACertPEM),
			KeyPEM:      []byte(body.CAKeyPEM),
			Fingerprint: fp[:],
			NotBefore:   cert.NotBefore,
			NotAfter:    cert.NotAfter,
		},
		Members: refs,
	}, nil
}

// caCertEntry mirrors one element of components/schemas/ClusterCA.cas in
// control-plane.yaml, decoding only the fields the prefetch needs.
type caCertEntry struct {
	CertPEM           string `json:"cert_pem"`
	FingerprintSHA256 string `json:"fingerprint_sha256"`
}

// caBundleResponse mirrors components/schemas/ClusterCA: the CA trust set
// plus the active signer's fingerprint.
type caBundleResponse struct {
	CAs                     []caCertEntry `json:"cas"`
	SignerFingerprintSHA256 string        `json:"signer_fingerprint_sha256"`
}

// prefetchPinnedCA fetches /v1/ca over a TOFU (InsecureSkipVerify) connection
// and returns a cert pool containing ONLY the operator-pinned CA. Every bundle
// entry is still checked for fingerprint self-consistency and the pinned
// fingerprint must be present, but no other entry lands in the pool: the
// bundle arrives over an unauthenticated channel, so an active MITM could
// append its own (self-consistent) CA alongside the real one - pooling the
// whole bundle would let an attacker-CA-signed leaf pass the verified
// /v1/cluster/join handshake and capture the cluster token. The pool then
// anchors the verified /v1/cluster/join POST so the token is never sent
// before the server is authenticated. The TLS skip is acceptable
// only here: no CA is pinned yet, the payload is public (CA certs only), and
// the fingerprint pin makes substitution detectable.
func prefetchPinnedCA(ctx context.Context, cpURL, expectedFingerprint string, timeout time.Duration) (*x509.CertPool, error) {
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, //nolint:gosec // TOFU anchor fetch - public payload, fingerprint-pinned below
				MinVersion:         tls.VersionTLS13,
			},
			Proxy: nil,
		},
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, cpURL+"/v1/ca", nil)
	if err != nil {
		return nil, fmt.Errorf("build /v1/ca request: %v", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch /v1/ca: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("fetch /v1/ca: HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(preview)))
	}

	var body caBundleResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode /v1/ca response: %v", err)
	}
	if len(body.CAs) == 0 {
		return nil, errors.New("/v1/ca response has empty cas bundle")
	}

	pool := x509.NewCertPool()
	pinned := false
	for i, entry := range body.CAs {
		if entry.CertPEM == "" {
			return nil, fmt.Errorf("/v1/ca cas[%d] has empty cert_pem", i)
		}
		cert, _, err := auth.ParseClusterCACert([]byte(entry.CertPEM))
		if err != nil {
			return nil, fmt.Errorf("parse /v1/ca cas[%d]: %v", i, err)
		}
		computed := sha256.Sum256(cert.Raw)
		computedHex := hex.EncodeToString(computed[:])
		reported := strings.ToLower(strings.TrimSpace(entry.FingerprintSHA256))
		if reported != computedHex {
			return nil, fmt.Errorf(
				"server-reported fingerprint %q for /v1/ca cas[%d] does not match computed %q",
				reported, i, computedHex)
		}
		if computedHex == expectedFingerprint {
			pinned = true
			if !pool.AppendCertsFromPEM([]byte(entry.CertPEM)) {
				return nil, fmt.Errorf("/v1/ca cas[%d] contains no usable PEM certificate", i)
			}
		}
	}
	if !pinned {
		return nil, fmt.Errorf(
			"operator-pinned fingerprint %s is not present in the /v1/ca bundle (%d CAs)",
			expectedFingerprint, len(body.CAs))
	}

	return pool, nil
}

// postClusterJoin POSTs the cluster token over a transport that verifies the
// target replica's serving cert against the prefetched, fingerprint-pinned
// cluster CA pool, and decodes the response, mapping a non-201 status to a
// descriptive error. Only the /v1/ca prefetch is TOFU; this token-bearing
// call is fully verified.
func postClusterJoin(ctx context.Context, p ClusterJoinFetchParams, caPool *x509.CertPool) (clusterJoinResponse, error) {
	reqBody, err := json.Marshal(map[string]string{"token": p.Token, "peer_url": p.PeerURL})
	if err != nil {
		return clusterJoinResponse{}, fmt.Errorf("marshal request: %v", err)
	}

	client := &http.Client{
		Timeout: p.Timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    caPool,
				MinVersion: tls.VersionTLS13,
			},
			Proxy: nil,
		},
		// Never follow redirects: a 307/308 would re-send the cluster token
		// body to whatever target the server names.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	reqCtx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, p.CPURL+"/v1/cluster/join", strings.NewReader(string(reqBody)))
	if err != nil {
		return clusterJoinResponse{}, fmt.Errorf("build /v1/cluster/join request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return clusterJoinResponse{}, fmt.Errorf("post /v1/cluster/join: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return clusterJoinResponse{}, fmt.Errorf("post /v1/cluster/join: HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(preview)))
	}

	var body clusterJoinResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return clusterJoinResponse{}, fmt.Errorf("decode /v1/cluster/join response: %v", err)
	}
	if body.CACertPEM == "" || body.CAKeyPEM == "" {
		return clusterJoinResponse{}, errors.New("/v1/cluster/join response missing ca_cert_pem or ca_key_pem")
	}
	return body, nil
}

// verifyKeyMatchesCert confirms the returned private key pairs with the CA
// cert's public key - defense against a tampered response that swaps the key.
func verifyKeyMatchesCert(keyPEM []byte, certPub crypto.PublicKey) error {
	key, err := auth.ParseClusterCAKey(keyPEM)
	if err != nil {
		return fmt.Errorf("parse cluster CA key: %v", err)
	}
	if err := auth.KeyMatchesCert(key, certPub); err != nil {
		return fmt.Errorf("cluster CA key: %v", err)
	}
	return nil
}

// normalizeCAFingerprint canonicalises the pinned fingerprint to 64-char
// lowercase hex, accepting an optional "sha256:" prefix and surrounding space.
func normalizeCAFingerprint(input string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(input))
	s = strings.TrimPrefix(s, "sha256:")
	if len(s) != 64 {
		return "", fmt.Errorf("ca_fingerprint must be 64 hex chars (optional sha256: prefix), got %q", input)
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", fmt.Errorf("ca_fingerprint must be hex, got %q", input)
		}
	}
	return s, nil
}
