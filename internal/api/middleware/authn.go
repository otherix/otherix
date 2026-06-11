// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
)

// Verifier is the narrow contract Authn needs from auth.Service.
// Defined here (consumer side) so the middleware tests can substitute
// a test double without standing up a real Service.
type Verifier interface {
	VerifyAccessToken(raw string) (*auth.User, error)
	VerifyAPIToken(ctx context.Context, raw string) (*auth.User, error)
}

// Authn returns a middleware that requires a valid bearer token on every
// downstream request. The token is dispatched by prefix: tokens starting
// with "otx_" are looked up in api_tokens; everything else is parsed as
// a JWT. Successful verification places an *auth.User in the request
// context; failure returns 401 in the standard error envelope.
//
// Authn does NOT enforce permissions — that is the future RBAC layer's
// job. A successful return only attests that the caller is who they
// claim to be.
func Authn(v Verifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok, ok := bearerToken(r)
			if !ok {
				response.WriteError(w, r, http.StatusUnauthorized,
					response.CodeUnauthenticated, "missing bearer token", nil)
				return
			}

			user, err := verify(r.Context(), v, tok)
			if err != nil {
				code, msg := authErrorToResponse(err)
				response.WriteError(w, r, http.StatusUnauthorized, code, msg, nil)
				return
			}

			ctx := auth.WithUser(r.Context(), user)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func verify(ctx context.Context, v Verifier, raw string) (*auth.User, error) {
	if auth.IsAPITokenFormat(raw) {
		return v.VerifyAPIToken(ctx, raw)
	}
	return v.VerifyAccessToken(raw)
}

func authErrorToResponse(err error) (response.ErrorCode, string) {
	switch {
	case errors.Is(err, auth.ErrTokenExpired):
		return response.CodeUnauthenticated, "token expired"
	case errors.Is(err, auth.ErrTokenRevoked):
		return response.CodeTokenRevoked, "token revoked"
	default:
		return response.CodeUnauthenticated, "invalid token"
	}
}

// bearerToken extracts the token portion of an Authorization: Bearer
// header. The scheme match is case-insensitive per RFC 7235; the token
// after the scheme is returned verbatim (the downstream otx_ prefix
// match is case-sensitive). Returns ("", false) when the header is
// absent, malformed, or has no token after the scheme.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	scheme, rest, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	tok := strings.TrimSpace(rest)
	if tok == "" {
		return "", false
	}
	return tok, true
}
