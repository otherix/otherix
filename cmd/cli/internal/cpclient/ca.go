// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cpclient

import (
	"context"
	"net/http"
)

// ClusterCACert mirrors components/schemas/ClusterCACert: one CA in the
// trust set. The CA private key is never part of this shape (server-side
// ca_certs.key_pem stays behind the API boundary).
type ClusterCACert struct {
	CertPEM           string `json:"cert_pem"`
	FingerprintSHA256 string `json:"fingerprint_sha256"`
	NotBefore         string `json:"not_before"`
	NotAfter          string `json:"not_after"`
}

// ClusterCA mirrors components/schemas/ClusterCA: the trust-set bundle plus
// the active signer's fingerprint.
type ClusterCA struct {
	CAs                     []ClusterCACert `json:"cas"`
	SignerFingerprintSHA256 string          `json:"signer_fingerprint_sha256"`
}

// GetCA fetches GET /v1/ca — the anonymous CA introspection endpoint
// used by agent bootstrap to learn the cluster CA trust set + the signer
// fingerprint. The request bypasses bearer-auth header injection by
// using newRequest under a Client constructed via NewAnonymous; an
// authenticated Client also works (server accepts both).
func (c *Client) GetCA(ctx context.Context) (ClusterCA, error) {
	httpReq, err := c.newRequest(ctx, http.MethodGet, "/v1/ca", nil)
	if err != nil {
		return ClusterCA{}, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return ClusterCA{}, err
	}
	var out ClusterCA
	if err := decodeJSON(body, &out); err != nil {
		return ClusterCA{}, err
	}
	return out, nil
}
