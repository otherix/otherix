// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cpclient

import (
	"context"
	"net/http"
)

// ClusterCA mirrors components/schemas/ClusterCA. The CA private key
// is never part of this shape (server-side ca_certs.key_pem stays
// behind the API boundary).
type ClusterCA struct {
	CertPEM           string `json:"cert_pem"`
	FingerprintSHA256 string `json:"fingerprint_sha256"`
	NotBefore         string `json:"not_before"`
	NotAfter          string `json:"not_after"`
}

// GetCA fetches GET /v1/ca — the anonymous CA introspection endpoint
// used by agent bootstrap к learn the cluster CA cert PEM + sha256
// fingerprint. The request bypasses bearer-auth header injection by
// using newRequest under а Client constructed via NewAnonymous; an
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
