// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cpclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// CreateAPITokenRequest is the body of POST .../api-tokens. The server
// has no `scopes` parameter - permission resolution is role-driven at
// request time, not at token creation. ExpiresAt, when non-nil, is an
// RFC3339 timestamp in the future; nil means a long-lived token.
type CreateAPITokenRequest struct {
	Name      string  `json:"name"`
	ExpiresAt *string `json:"expires_at,omitempty"`
}

// APIToken is the server projection of an api_tokens row. Token is the
// plaintext, returned exactly once on creation; it is omitempty so list
// output (which never carries a plaintext) does not emit an empty
// "token" field. Prefix is the first 8 chars of the plaintext
// (`otx_xxxx`) and identifies a token without leaking the secret.
type APIToken struct {
	ID         string  `json:"id"`
	UserID     string  `json:"user_id"`
	Name       string  `json:"name"`
	Prefix     string  `json:"prefix"`
	Token      string  `json:"token,omitempty"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at"`
	ExpiresAt  *string `json:"expires_at"`
	RevokedAt  *string `json:"revoked_at"`
}

// ListAPITokensParams collects the query knobs GET .../api-tokens
// accepts. IncludeRevoked surfaces revoked rows (hidden by default);
// Limit / Cursor drive cursor pagination.
type ListAPITokensParams struct {
	Limit          int
	Cursor         string
	IncludeRevoked bool
}

// APITokenList is the cursor-paginated payload of GET .../api-tokens.
type APITokenList struct {
	Data []APIToken `json:"data"`
	Meta struct {
		NextCursor *string `json:"next_cursor"`
	} `json:"meta"`
}

// apiTokensPath builds the collection path for userID. An empty userID
// targets the calling user's own collection (/v1/users/me/api-tokens);
// a non-empty UUID targets that user's collection (admin-on-behalf).
func apiTokensPath(userID string) string {
	if userID == "" {
		return "/v1/users/me/api-tokens"
	}
	return "/v1/users/" + userID + "/api-tokens"
}

// CreateAPITokenFor mints a token for the target user. userID == ""
// targets the calling user. The create view carries the one-time
// plaintext in Token.
func (c *Client) CreateAPITokenFor(ctx context.Context, userID string, in CreateAPITokenRequest) (APIToken, error) {
	req, err := c.newRequest(ctx, http.MethodPost, apiTokensPath(userID), in)
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

// ListAPITokensFor fetches one page of the target user's tokens.
// userID == "" targets the calling user. Pagination is the caller's
// responsibility (re-issue with APITokenList.Meta.NextCursor).
func (c *Client) ListAPITokensFor(ctx context.Context, userID string, p ListAPITokensParams) (APITokenList, error) {
	q := url.Values{}
	if p.Limit > 0 {
		q.Set("limit", strconv.Itoa(p.Limit))
	}
	if p.Cursor != "" {
		q.Set("cursor", p.Cursor)
	}
	if p.IncludeRevoked {
		q.Set("include_revoked", "true")
	}
	path := apiTokensPath(userID)
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return APITokenList{}, err
	}
	_, body, err := c.do(req)
	if err != nil {
		return APITokenList{}, err
	}
	var out APITokenList
	if err := decodeJSON(body, &out); err != nil {
		return APITokenList{}, err
	}
	return out, nil
}

// RevokeAPITokenFor revokes token tokenID under the target user.
// userID == "" targets the calling user. The server's DELETE is
// idempotent (204 on both fresh-revoke and already-revoked).
func (c *Client) RevokeAPITokenFor(ctx context.Context, userID, tokenID string) error {
	req, err := c.newRequest(ctx, http.MethodDelete, apiTokensPath(userID)+"/"+tokenID, nil)
	if err != nil {
		return err
	}
	_, _, err = c.do(req)
	return err
}

// CreateAPIToken mints a token under the calling user's account. Thin
// self-path wrapper retained for `config add cluster`.
func (c *Client) CreateAPIToken(ctx context.Context, in CreateAPITokenRequest) (APIToken, error) {
	return c.CreateAPITokenFor(ctx, "", in)
}

// RevokeAPIToken revokes one of the calling user's tokens by id. Thin
// self-path wrapper retained for `config add cluster --force`.
func (c *Client) RevokeAPIToken(ctx context.Context, tokenID string) error {
	return c.RevokeAPITokenFor(ctx, "", tokenID)
}
