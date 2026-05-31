// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package api

import (
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/tls"
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

// FetchClusterCA performs the joiner's bootstrap round-trip: POST the cluster
// token to /v1/cluster/join over a TOFU TLS connection (the target replica's
// serving cert does not chain to the CA we are fetching), then verify the
// returned CA against the operator-pinned fingerprint and confirm the key pairs
// with the cert. Returns the CA material to persist on disk before etcd starts.
//
// TLS uses InsecureSkipVerify: the payload is self-verifying via the
// fingerprint pin (the same TOFU model as agent bootstrap), so transport
// authentication is not relied upon.
func FetchClusterCA(ctx context.Context, p ClusterJoinFetchParams, log *slog.Logger) (ClusterJoinResult, error) {
	expected, err := normalizeCAFingerprint(p.CAFingerprint)
	if err != nil {
		return ClusterJoinResult{}, err
	}

	body, err := postClusterJoin(ctx, p)
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

// postClusterJoin issues the TOFU POST and decodes the response, mapping a
// non-201 status to a descriptive error.
func postClusterJoin(ctx context.Context, p ClusterJoinFetchParams) (clusterJoinResponse, error) {
	reqBody, err := json.Marshal(map[string]string{"token": p.Token, "peer_url": p.PeerURL})
	if err != nil {
		return clusterJoinResponse{}, fmt.Errorf("marshal request: %v", err)
	}

	client := &http.Client{
		Timeout: p.Timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, //nolint:gosec // TOFU - security from the post-receipt CA fingerprint pin
				MinVersion:         tls.VersionTLS12,
			},
			Proxy: nil,
		},
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
