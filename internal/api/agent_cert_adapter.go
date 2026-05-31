// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package api

import (
	"context"
	"fmt"

	"github.com/otherix/otherix/internal/auth"
)

// agentCertAdapter lifts an AgentCertLooker (the fingerprint -> agent-cert
// lookup both backends expose) into auth.AgentCertLookupResult so
// auth.AgentVerifier stays decoupled from the storage backend. Held inside
// internal/api because the wiring already pairs storage with auth here.
type agentCertAdapter struct {
	s AgentCertLooker
}

// LookupAgentCertByFingerprint satisfies auth.AgentCertQuerier.
func (a *agentCertAdapter) LookupAgentCertByFingerprint(ctx context.Context, fingerprint []byte) (auth.AgentCertLookupResult, error) {
	cert, err := a.s.AgentCertByFingerprint(ctx, fingerprint)
	if err != nil {
		return auth.AgentCertLookupResult{}, fmt.Errorf("agent cert lookup: %w", err)
	}
	return auth.AgentCertLookupResult{
		NodeID:  cert.NodeID,
		Revoked: cert.RevokedAt != nil,
	}, nil
}

func newAgentVerifier(s AgentCertLooker) *auth.AgentVerifier {
	return auth.NewAgentVerifier(&agentCertAdapter{s: s})
}
