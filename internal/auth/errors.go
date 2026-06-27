// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package auth implements the api-server's authentication primitives:
// argon2id password hashing, HS256 JWT issuance and verification, opaque
// refresh tokens with rotation, otx_-prefixed API tokens, and the Service
// type that ties them together against the user/refresh_token/api_token
// tables. HTTP wiring lives in internal/api; this package owns no
// HTTP-shaped concerns.
package auth

import "errors"

// Sentinel errors returned by Service and the verification primitives.
// HTTP handlers map these to error codes via type-switch on errors.Is.
var (
	// ErrInvalidCredentials is returned by Login when no user matches
	// the provided username or the password fails verification. The two
	// cases are intentionally indistinguishable to the caller — the
	// login endpoint must not leak which usernames are registered.
	ErrInvalidCredentials = errors.New("invalid credentials")

	// ErrTokenExpired is returned when a JWT, refresh, or API token is
	// well-formed and unrevoked but past its expiry.
	ErrTokenExpired = errors.New("token expired")

	// ErrInvalidToken is returned when a token is malformed, has a bad
	// signature, references a non-existent or soft-deleted user, or is
	// otherwise structurally unusable.
	ErrInvalidToken = errors.New("invalid token")

	// ErrTokenRevoked is returned when a token's revoked_at is set.
	ErrTokenRevoked = errors.New("token revoked")

	// ErrTokenReplay signals refresh-token theft: a revoked refresh
	// token was presented at /v1/auth/refresh. Service.Refresh revokes
	// the entire family before returning this error.
	ErrTokenReplay = errors.New("refresh token replay detected")

	// ErrPermissionDenied signals that an authenticated principal does
	// not hold a permission required for the requested action, or that
	// a scope=own check failed. HTTP layers map it to 403
	// permission_denied; nothing else maps to 403 from this package.
	ErrPermissionDenied = errors.New("permission denied")
)
