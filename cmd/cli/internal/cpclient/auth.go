// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cpclient

import (
	"context"
	"net/http"
)

// LoginRequest is the body of POST /v1/auth/login. Mirrors
// loginRequest in internal/api/handlers/auth.
type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// LoginResponse mirrors the loginResponse shape on the CP. The user
// subobject is intentionally omitted — `config add cluster` only
// needs the access token, and keeping the wire-type lean keeps the
// CLI binary's JSON surface small.
type LoginResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
}

// Login exchanges email + password for an access / refresh pair.
// May be called on an anonymous Client (NewAnonymous); the
// returned access token is the input to WithToken for the second
// hop (CreateAPIToken).
func (c *Client) Login(ctx context.Context, in LoginRequest) (LoginResponse, error) {
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/auth/login", in)
	if err != nil {
		return LoginResponse{}, err
	}
	_, body, err := c.do(req)
	if err != nil {
		return LoginResponse{}, err
	}
	var out LoginResponse
	if err := decodeJSON(body, &out); err != nil {
		return LoginResponse{}, err
	}
	return out, nil
}

// CreateAPITokenRequest is the body of POST /v1/users/me/api-tokens.
// Mirrors ApiTokenCreate in api/openapi/control-plane.yaml — note
// the server has no `scopes` parameter; permission resolution is
// role-driven and happens at request time, not at token-creation
// time. ExpiresAt, when non-nil, is an RFC3339 string in the
// future; nil means "long-lived" (the CLI's default for cluster
// tokens, which we expect to outlive any single session).
type CreateAPITokenRequest struct {
	Name      string  `json:"name"`
	ExpiresAt *string `json:"expires_at,omitempty"`
}

// APIToken is the full server projection of an api_tokens row.
// Token is the plaintext, returned exactly once on creation; the
// CLI stores it in the config file. Prefix is the first 12 chars
// of plaintext (`otx_<6 chars>`) and lets us identify a token in
// `config show --show-token=false` without leaking the secret.
type APIToken struct {
	ID         string  `json:"id"`
	UserID     string  `json:"user_id"`
	Name       string  `json:"name"`
	Prefix     string  `json:"prefix"`
	Token      string  `json:"token"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at"`
	ExpiresAt  *string `json:"expires_at"`
	RevokedAt  *string `json:"revoked_at"`
}

// CreateAPIToken creates an API token under the calling user's
// account. The Client must be authenticated with a JWT (typically
// freshly minted via Login).
func (c *Client) CreateAPIToken(ctx context.Context, in CreateAPITokenRequest) (APIToken, error) {
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/users/me/api-tokens", in)
	if err != nil {
		return APIToken{}, err
	}
	_, body, err := c.do(req)
	if err != nil {
		return APIToken{}, err
	}
	var out APIToken
	if err := decodeJSON(body, &out); err != nil {
		return APIToken{}, err
	}
	return out, nil
}

// RevokeAPIToken revokes one of the calling user's API tokens by
// id. The CP's DELETE /v1/users/me/api-tokens/{token_id} is
// idempotent (204 on both fresh-revoke and already-revoked). Used
// by `config add cluster --force` to retire the previous cluster's
// token before overwriting the entry.
func (c *Client) RevokeAPIToken(ctx context.Context, tokenID string) error {
	req, err := c.newRequest(ctx, http.MethodDelete, "/v1/users/me/api-tokens/"+tokenID, nil)
	if err != nil {
		return err
	}
	_, _, err = c.do(req)
	return err
}
