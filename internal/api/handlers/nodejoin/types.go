// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package nodejoin

// joinRequest is the body of POST /v1/nodes/join. Mirrors
// components/schemas/NodeJoinRequest в control-plane.yaml. All fields
// required by Step 2 contract — the server creates the nodes row если
// it doesn't exist yet, so the agent must supply identity (name,
// architecture) и migration ingress upfront.
//
// Pointer types where useful for tri-state decoding в later iterations;
// Step 2 keeps the simple required-non-empty contract.
type joinRequest struct {
	Token                   string `json:"token"`
	CSRPEM                  string `json:"csr_pem"`
	NodeName                string `json:"node_name"`
	Architecture            string `json:"architecture"`
	AdvertisedEndpoint      string `json:"advertised_endpoint"`
	MigrationHost           string `json:"migration_host"`
	MigrationPortRangeStart int32  `json:"migration_port_range_start"`
	MigrationPortRangeEnd   int32  `json:"migration_port_range_end"`
}

// joinResponse is the 201 envelope. The plaintext leaf cert is
// returned exactly once — agent persists locally on disk и uses it
// для каждый subsequent mTLS handshake. CACertPEM is included so the
// agent can also persist the trust anchor от the same response (and
// optionally compare against the fingerprint it pinned at bootstrap
// time, defense-in-depth).
type joinResponse struct {
	NodeID    string `json:"node_id"`
	CertPEM   string `json:"cert_pem"`
	CACertPEM string `json:"ca_cert_pem"`
}
