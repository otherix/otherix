// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package middleware

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
)

// AgentVerifier is the narrow contract AgentMTLS needs from the auth
// package. Defined here (consumer side) so the middleware tests can
// substitute a test double without standing up a real store.
type AgentVerifier interface {
	VerifyFingerprint(ctx context.Context, fingerprint []byte) (*auth.Agent, error)
}

// AgentMTLS gates a route subtree on a verified client certificate
// whose SHA-256 fingerprint resolves to a registered, unrevoked
// agent_certs row. The bound node id is placed in the request
// context as an *auth.Agent.
//
// On any failure the middleware writes a 401 with the standard
// error envelope and a `details.reason` field naming one of
// `cert_untrusted`, `cert_san_unknown`, or `cert_revoked` per the
// `agentMTLS` scheme in api/openapi/control-plane.yaml.
//
// AgentMTLS does NOT verify the certificate chain against the
// cluster CA itself. The agent-facing listener is configured with
// `tls.Config.ClientAuth = tls.VerifyClientCertIfGiven` and a
// CA-anchored ClientCAs pool (see buildAgentServerTLSConfig in
// internal/api/server.go): a presented client cert IS chain-verified
// during the handshake, but connections without any client cert are
// also accepted, because the same listener serves the anonymous
// bootstrap routes (`/v1/ca`, `/v1/nodes/join`). This middleware
// supplies the "require" half per route subtree: it rejects requests
// that arrived without a client certificate, and binds the (already
// chain-verified) cert's fingerprint to a registered, unrevoked agent.
func AgentMTLS(v AgentVerifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
				writeMTLSError(w, r, "cert_untrusted",
					"client certificate required")
				return
			}
			cert := r.TLS.PeerCertificates[0]
			fp := sha256.Sum256(cert.Raw)

			agent, err := v.VerifyFingerprint(r.Context(), fp[:])
			if err != nil {
				switch {
				case errors.Is(err, auth.ErrCertUnknown):
					writeMTLSError(w, r, "cert_san_unknown",
						"client certificate is not bound to a registered agent")
				case errors.Is(err, auth.ErrCertRevoked):
					writeMTLSError(w, r, "cert_revoked",
						"client certificate has been revoked")
				default:
					response.WriteError(w, r, http.StatusInternalServerError,
						response.CodeInternal, "agent identity lookup failed", nil)
				}
				return
			}

			ctx := auth.WithAgent(r.Context(), agent)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func writeMTLSError(w http.ResponseWriter, r *http.Request, reason, message string) {
	response.WriteError(w, r, http.StatusUnauthorized,
		response.CodeUnauthenticated, message,
		map[string]any{"reason": reason})
}
