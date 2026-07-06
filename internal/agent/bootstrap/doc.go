// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package bootstrap implements the agent side of the join-token
// bootstrap protocol. On a freshly-installed agent with no cert material
// on disk, Bootstrap fetches the cluster CA via /v1/ca, verifies its
// fingerprint against an operator-pinned value (TOFU), generates an
// ECDSA P-384 keypair, builds a CSR, and redeems the supplied join
// token at /v1/nodes/join. The returned cert + key + CA + node-id
// are then persisted to disk atomically.
//
// TLS posture: only the GET /v1/ca anchor fetch uses
// `InsecureSkipVerify: true` - at that first call no cluster CA is
// pinned yet, so there is nothing to verify the CP serving cert
// against; the payload is public (CA certificates only); and the
// operator fingerprint pin makes any MITM substitution detectable.
// Once /v1/ca has pinned the cluster CA, the token-bearing
// POST /v1/nodes/join verifies the CP serving cert against that
// pinned CA before the token leaves the agent, so a MITM cannot
// capture the token. Two cryptographic checks on the returned
// material complete the binding:
//
//  1. sha256(returned_ca_cert.Raw) must equal the operator-pinned
//     fingerprint.
//  2. The leaf cert returned by /v1/nodes/join must chain back to the
//     same CA whose fingerprint just matched.
//
// Both checks must pass before any file is written to disk. Failures
// short-circuit with descriptive errors — see the FingerprintMismatchError
// and ErrCSRRejected sentinels.
//
// Bootstrap is re-runnable across a lost join response. If
// /v1/nodes/join returns 201 but the response never reaches the agent
// (network drop, process kill), re-running with the SAME token recovers:
// CP-side redemption is re-runnable for a node that has not yet
// heartbeated - it supersedes the undelivered cert and re-issues without
// spending additional max_uses. A fresh token is required only once the
// node has confirmed ownership by heartbeating.
package bootstrap
