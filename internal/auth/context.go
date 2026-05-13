// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import (
	"context"

	"github.com/google/uuid"
)

// Type is the kind of credential the request was authenticated with.
// It distinguishes a JWT (interactive session) from an API token
// (long-lived programmatic credential), which lets handlers and audit
// logging treat them differently.
type Type string

// Authentication types.
const (
	TypeJWT      Type = "jwt"
	TypeAPIToken Type = "api_token"
)

// User is the authenticated principal attached to an in-flight request.
// It is the minimum needed by handlers and the RBAC middleware; fuller
// user data is loaded on demand via the store.
type User struct {
	ID   uuid.UUID
	Role Role
	Type Type

	// JWTID is the access token's `jti` claim when Type == TypeJWT,
	// nil otherwise. Reserved for a future blacklist; today the field
	// is purely informational.
	JWTID *uuid.UUID

	// APITokenID is the api_tokens.id when Type == TypeAPIToken,
	// nil otherwise. Used by audit logging to identify the credential.
	APITokenID *uuid.UUID
}

type ctxKey struct{}

// WithUser returns a derived context carrying u as the authenticated
// principal. The authn middleware calls WithUser exactly once per
// successful request.
func WithUser(ctx context.Context, u *User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

// UserFromContext returns the principal placed by WithUser, or nil for
// anonymous requests. Handlers behind the authn middleware can assume a
// non-nil result; handlers exposed without it must nil-check.
func UserFromContext(ctx context.Context) *User {
	u, _ := ctx.Value(ctxKey{}).(*User)
	return u
}
