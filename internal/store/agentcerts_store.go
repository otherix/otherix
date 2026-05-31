// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import "context"

// AgentCertByFingerprint returns the agent cert matching the SHA-256
// fingerprint, or ErrNotFound. Only NodeID and RevokedAt are populated - the
// lookup backs the agent-mTLS fingerprint -> node binding, which needs nothing
// else. Exposing it as a direct method (not via Queries) lets the router depend
// on the storage backend through an interface both backends satisfy.
func (s *Store) AgentCertByFingerprint(ctx context.Context, fingerprint []byte) (AgentCert, error) {
	row, err := s.queries.LookupAgentCertByFingerprint(ctx, fingerprint)
	if err != nil {
		return AgentCert{}, translateNoRows(err)
	}
	return AgentCert{NodeID: row.NodeID, RevokedAt: row.RevokedAt}, nil
}
