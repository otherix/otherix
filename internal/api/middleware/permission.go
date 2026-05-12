// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package middleware

import (
	"log/slog"
	"net/http"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
)

// RequirePermission returns a middleware that gates the wrapped handler
// on perm. It must run after Authn — without an authenticated principal
// in the context the middleware returns 401, since "no role" precedes
// "role lacks this".
//
// Insufficient permission yields 403 permission_denied with
// details.required_permission set to perm and a WARN log carrying
// user_id, role, required_permission, path, and request_id.
//
// RequirePermission only checks role-level capability. Scope (own vs
// any) is the handler's job via auth.CheckOwnership.
func RequirePermission(perm auth.Permission, log *slog.Logger) func(http.Handler) http.Handler {
	permStr := string(perm)
	deniedDetails := map[string]any{"required_permission": permStr}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user := auth.UserFromContext(r.Context())
			if user == nil {
				response.WriteError(w, r, http.StatusUnauthorized,
					response.CodeUnauthenticated, "authentication required", nil)
				return
			}

			if !auth.Has(user.Role, perm) {
				log.WarnContext(r.Context(), "permission denied",
					slog.String("user_id", user.ID.String()),
					slog.String("role", string(user.Role)),
					slog.String("required_permission", permStr),
					slog.String("path", r.URL.Path),
					slog.String("request_id", RequestIDFromContext(r.Context())),
				)
				response.WriteError(w, r, http.StatusForbidden,
					response.CodePermissionDenied, "permission denied", deniedDetails)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
