// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// jwtIssuer is asserted into every issued token's `iss` claim and
// required to match on parse. Hard-coding it avoids one more thing
// the operator has to keep in sync between binaries.
const jwtIssuer = "otherix"

// AccessClaims is the payload of an access JWT. The standard registered
// claims (`sub`, `iss`, `iat`, `nbf`, `exp`, `jti`) come from the
// embedded RegisteredClaims; `role` is project-specific so the authn
// middleware can route requests to RBAC without a DB hit.
type AccessClaims struct {
	Role string `json:"role"`
	jwt.RegisteredClaims
}

// IssueAccessToken returns a signed access JWT for userID with role
// embedded in the `role` claim. The token is valid for ttl from now.
// Each call uses a fresh `jti` (UUIDv4). role is taken as a typed
// Role rather than a raw string to keep the only place that mints a
// JWT inside the type system.
func IssueAccessToken(secret []byte, userID uuid.UUID, role Role, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := AccessClaims{
		Role: string(role),
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			Issuer:    jwtIssuer,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			ID:        uuid.NewString(),
		},
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
	if err != nil {
		return "", fmt.Errorf("sign access token: %v", err)
	}
	return signed, nil
}

// ParseAccessToken verifies signature, issuer, and expiry of raw and
// returns the decoded claims. Expiry mismatches map to ErrTokenExpired;
// every other parse failure (bad signature, wrong issuer, malformed
// payload, mismatched signing algorithm) maps to ErrInvalidToken so
// callers can keep one branch for "token is no good".
func ParseAccessToken(secret []byte, raw string) (*AccessClaims, error) {
	parsed, err := jwt.ParseWithClaims(raw, &AccessClaims{}, func(t *jwt.Token) (any, error) {
		// Reject `alg: none` and asymmetric algorithms — we only sign HS256.
		if t.Method.Alg() != jwt.SigningMethodHS256.Alg() {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return secret, nil
	},
		jwt.WithIssuer(jwtIssuer),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrTokenExpired
		}
		return nil, ErrInvalidToken
	}

	claims, ok := parsed.Claims.(*AccessClaims)
	if !ok || !parsed.Valid {
		return nil, ErrInvalidToken
	}
	return claims, nil
}
