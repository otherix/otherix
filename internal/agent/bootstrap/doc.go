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
// First-request TLS posture is `InsecureSkipVerify: true` - the CP's
// TLS server certificate is not necessarily issued by the cluster CA
// we are bootstrapping against, so the standard chain-of-trust
// mechanism cannot bind the two requests. Security instead comes
// from two cryptographic checks performed after the network
// round-trips complete:
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
// Bootstrap is NOT idempotent across successful token redemption. If
// /v1/nodes/join returns 201 but the response never reaches the agent
// (network drop, process kill), the token is consumed at the CP side
// and the operator must mint a fresh one to retry.
package bootstrap
