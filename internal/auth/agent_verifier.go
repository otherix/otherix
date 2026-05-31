// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// AgentCertQuerier is the narrow store contract AgentVerifier
// consumes. Implemented by *store.Queries; declared here so
// AgentVerifier can be tested with a fake without pulling the store
// package into auth.
type AgentCertQuerier interface {
	LookupAgentCertByFingerprint(ctx context.Context, fingerprint []byte) (AgentCertLookupResult, error)
}

// AgentCertLookupResult is the pared-down view AgentVerifier needs.
// An adapter in the router wiring lifts the sqlc-generated row into
// this shape so AgentVerifier itself stays decoupled from store.
type AgentCertLookupResult struct {
	NodeID  uuid.UUID
	Revoked bool
}

// Agent-side authentication sentinels. The agentMTLS middleware
// type-switches on these to choose the `details.reason` field of
// the 401 envelope per `api/openapi/control-plane.yaml` agentMTLS
// scheme.
var (
	// ErrCertUnknown is returned when the SHA-256 fingerprint does
	// not match any agent_certs row. Maps to 401 cert_san_unknown.
	ErrCertUnknown = errors.New("agent certificate is not registered")

	// ErrCertRevoked is returned when the matching agent_certs row
	// has revoked_at set. Maps to 401 cert_revoked.
	ErrCertRevoked = errors.New("agent certificate has been revoked")
)

// AgentVerifier resolves a verified client-cert fingerprint to an
// *Agent or one of the sentinel errors above. It does NOT verify
// the certificate chain — that is the TLS listener's job (configured
// with RequireAndVerifyClientCert + a CA-anchored ClientCAs pool).
type AgentVerifier struct {
	q AgentCertQuerier
}

// NewAgentVerifier constructs an AgentVerifier backed by q. q is
// typically a thin adapter around *store.Queries.
func NewAgentVerifier(q AgentCertQuerier) *AgentVerifier {
	return &AgentVerifier{q: q}
}

// VerifyFingerprint looks up an agent_certs row by SHA-256
// fingerprint (32 raw bytes). Returns ErrCertUnknown on no match,
// ErrCertRevoked when the row's revoked_at is set, an underlying
// error on database failure, or *Agent on success.
func (v *AgentVerifier) VerifyFingerprint(ctx context.Context, fingerprint []byte) (*Agent, error) {
	row, err := v.q.LookupAgentCertByFingerprint(ctx, fingerprint)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrCertUnknown
		}
		return nil, err
	}
	if row.Revoked {
		return nil, ErrCertRevoked
	}
	return &Agent{NodeID: row.NodeID, CertFingerprint: fingerprint}, nil
}
