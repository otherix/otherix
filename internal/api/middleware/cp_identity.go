// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package middleware

import (
	"log/slog"
	"net/http"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
)

// RequireCPIdentity gates an agent-served route subtree on a verified client
// certificate whose Subject CommonName is the control plane's
// auth.CPCertCommonName. The agent listener is tls.RequireAndVerifyClientCert
// with a cluster-CA ClientCAs pool, so any cert reaching this middleware is
// already chain-verified; this supplies the missing identity check (audit H1):
// without it a stolen node-<name> cert - itself a valid cluster-CA client cert -
// could drive a peer agent's full control surface.
//
// Rejection is 403 permission_denied with details.reason="cp_identity_required"
// and a WARN log carrying the presented common_name, path, and request_id. A
// missing client cert (defensive: the RequireAndVerifyClientCert listener should
// already have failed the handshake) is treated the same way. The 403 envelope
// is identical for the wrong-CN and no-cert cases so it carries no identity
// oracle.
func RequireCPIdentity(log *slog.Logger) func(http.Handler) http.Handler {
	deniedDetails := map[string]any{"reason": "cp_identity_required"}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
				log.WarnContext(r.Context(), "cp identity required: no client certificate",
					slog.String("path", r.URL.Path),
					slog.String("request_id", RequestIDFromContext(r.Context())),
				)
				response.WriteError(w, r, http.StatusForbidden,
					response.CodePermissionDenied,
					"control plane identity required", deniedDetails)
				return
			}

			cn := r.TLS.PeerCertificates[0].Subject.CommonName
			if cn != auth.CPCertCommonName {
				log.WarnContext(r.Context(), "cp identity required: client cert is not the control plane",
					slog.String("common_name", cn),
					slog.String("path", r.URL.Path),
					slog.String("request_id", RequestIDFromContext(r.Context())),
				)
				response.WriteError(w, r, http.StatusForbidden,
					response.CodePermissionDenied,
					"control plane identity required", deniedDetails)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
