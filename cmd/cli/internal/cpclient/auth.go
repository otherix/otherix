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
	Username string `json:"username"`
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

// Login exchanges username + password for an access / refresh pair.
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
