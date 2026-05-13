// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import (
	"context"

	"github.com/google/uuid"
)

// Agent is the mTLS-authenticated principal for node-agent traffic.
// Agents are a separate caller class from User: they do not flow
// through the role/permission matrix (RBAC governs human users and
// API tokens), authentication is by client certificate fingerprint,
// and the only fact handlers need is the bound node identity.
type Agent struct {
	NodeID uuid.UUID

	// CertFingerprint is the SHA-256 digest of the client cert that
	// authenticated this request, in raw bytes (32 long). Carried so
	// downstream code (audit logs, future certificate-rotation
	// flows) can reference the exact credential; handlers do not
	// need to consult the database for it.
	CertFingerprint []byte
}

type agentCtxKey struct{}

// WithAgent returns a derived context carrying a as the
// mTLS-authenticated principal. The agentMTLS middleware calls
// WithAgent exactly once per successful request.
func WithAgent(ctx context.Context, a *Agent) context.Context {
	return context.WithValue(ctx, agentCtxKey{}, a)
}

// AgentFromContext returns the principal placed by WithAgent, or nil
// when the request did not pass through the agentMTLS middleware.
// Handlers behind that middleware can assume a non-nil result; a nil
// here is a programmer error (route mounted without the middleware).
func AgentFromContext(ctx context.Context) *Agent {
	a, _ := ctx.Value(agentCtxKey{}).(*Agent)
	return a
}
