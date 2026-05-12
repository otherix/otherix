// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package api

import (
	"context"
	"fmt"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// agentCertAdapter lifts the sqlc-generated
// LookupAgentCertByFingerprintRow into auth.AgentCertLookupResult so
// auth.AgentVerifier stays decoupled from the concrete store package.
// Held inside internal/api because the wiring already pairs with
// concrete *store types here.
type agentCertAdapter struct {
	q *store.Queries
}

// LookupAgentCertByFingerprint satisfies auth.AgentCertQuerier.
func (a *agentCertAdapter) LookupAgentCertByFingerprint(ctx context.Context, fingerprint []byte) (auth.AgentCertLookupResult, error) {
	row, err := a.q.LookupAgentCertByFingerprint(ctx, fingerprint)
	if err != nil {
		return auth.AgentCertLookupResult{}, fmt.Errorf("agent cert lookup: %w", err)
	}
	return auth.AgentCertLookupResult{
		NodeID:  row.NodeID,
		Revoked: row.RevokedAt != nil,
	}, nil
}

func newAgentVerifier(q *store.Queries) *auth.AgentVerifier {
	return auth.NewAgentVerifier(&agentCertAdapter{q: q})
}
