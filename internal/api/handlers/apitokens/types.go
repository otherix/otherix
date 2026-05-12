// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package apitokens

// createRequest is the body of POST /v1/users/me/api-tokens and POST
// /v1/users/{id}/api-tokens. Mirrors ApiTokenCreate in
// api/openapi/control-plane.yaml.
type createRequest struct {
	Name      string  `json:"name"`
	ExpiresAt *string `json:"expires_at"`
}

// tokenView is the public projection of an api_tokens row. The
// plaintext and the storage hash are NEVER part of this shape.
type tokenView struct {
	ID         string  `json:"id"`
	UserID     string  `json:"user_id"`
	Name       string  `json:"name"`
	Prefix     string  `json:"prefix"`
	LastUsedAt *string `json:"last_used_at"`
	ExpiresAt  *string `json:"expires_at"`
	RevokedAt  *string `json:"revoked_at"`
	CreatedAt  string  `json:"created_at"`
}

// createResponse extends tokenView with the plaintext token. The
// plaintext is returned exactly once, on creation.
type createResponse struct {
	tokenView
	Token string `json:"token"`
}

// listResponse is the cursor-paginated list shape.
type listResponse struct {
	Data []tokenView    `json:"data"`
	Meta paginationMeta `json:"meta"`
}

// paginationMeta carries the opaque next_cursor; nil when the current
// page is the last.
type paginationMeta struct {
	NextCursor *string `json:"next_cursor"`
}
