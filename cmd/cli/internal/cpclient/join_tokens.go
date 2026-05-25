// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cpclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/google/uuid"
)

// JoinToken mirrors components/schemas/JoinToken in
// api/openapi/control-plane.yaml. The plaintext token is NEVER part
// of this shape — it surfaces only once in CreateJoinTokenResponse.
type JoinToken struct {
	ID               uuid.UUID  `json:"id"`
	IntendedNodeName *string    `json:"intended_node_name"`
	ExpiresAt        string     `json:"expires_at"`
	MaxUses          *int64     `json:"max_uses"`
	ConsumptionCount int64      `json:"consumption_count"`
	CreatedByUserID  *uuid.UUID `json:"created_by_user_id"`
	CreatedAt        string     `json:"created_at"`
}

// JoinTokenList is the cursor-paginated envelope of
// GET /v1/nodes/join-tokens.
type JoinTokenList struct {
	Data []JoinToken `json:"data"`
	Meta struct {
		NextCursor *string `json:"next_cursor"`
	} `json:"meta"`
}

// CreateJoinTokenRequest mirrors components/schemas/JoinTokenCreate.
// All fields optional; server applies defaults (ttl=1h, max_uses=null).
type CreateJoinTokenRequest struct {
	IntendedNodeName *string `json:"intended_node_name,omitempty"`
	TTLSeconds       *int    `json:"ttl_seconds,omitempty"`
	MaxUses          *int32  `json:"max_uses,omitempty"`
}

// CreateJoinTokenResponse mirrors JoinTokenCreateResponse — the join
// token row projection PLUS the plaintext token and active cluster CA
// fingerprint. The "token bundle" returned exactly once on creation.
type CreateJoinTokenResponse struct {
	JoinToken
	Token               string `json:"token"`
	CAFingerprintSHA256 string `json:"ca_fingerprint_sha256"`
}

// ListJoinTokensParams collects the query knobs of GET
// /v1/nodes/join-tokens.
type ListJoinTokensParams struct {
	Limit          int
	Cursor         string
	IncludeExpired bool
}

// CreateJoinToken submits POST /v1/nodes/join-tokens. 201 returns the
// parsed token bundle; any non-2xx surfaces as *APIError.
func (c *Client) CreateJoinToken(ctx context.Context, req CreateJoinTokenRequest) (CreateJoinTokenResponse, error) {
	httpReq, err := c.newRequest(ctx, http.MethodPost, "/v1/nodes/join-tokens", req)
	if err != nil {
		return CreateJoinTokenResponse{}, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return CreateJoinTokenResponse{}, err
	}
	var out CreateJoinTokenResponse
	if err := decodeJSON(body, &out); err != nil {
		return CreateJoinTokenResponse{}, err
	}
	return out, nil
}

// ListJoinTokens fetches GET /v1/nodes/join-tokens with the supplied
// filter knobs. Pagination is the caller's responsibility (re-issue with
// JoinTokenList.Meta.NextCursor on Params.Cursor).
func (c *Client) ListJoinTokens(ctx context.Context, params ListJoinTokensParams) (JoinTokenList, error) {
	q := url.Values{}
	if params.Limit > 0 {
		q.Set("limit", strconv.Itoa(params.Limit))
	}
	if params.Cursor != "" {
		q.Set("cursor", params.Cursor)
	}
	if params.IncludeExpired {
		q.Set("include_expired", "true")
	}

	path := "/v1/nodes/join-tokens"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}

	httpReq, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return JoinTokenList{}, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return JoinTokenList{}, err
	}
	var out JoinTokenList
	if err := decodeJSON(body, &out); err != nil {
		return JoinTokenList{}, err
	}
	return out, nil
}

// DeleteJoinToken submits DELETE /v1/nodes/join-tokens/{id}. 204
// returns nil; 404 / 409 / any non-2xx surfaces as *APIError so
// callers can errors.As on StatusCode.
func (c *Client) DeleteJoinToken(ctx context.Context, id uuid.UUID) error {
	httpReq, err := c.newRequest(ctx, http.MethodDelete, "/v1/nodes/join-tokens/"+id.String(), nil)
	if err != nil {
		return err
	}
	_, _, err = c.do(httpReq)
	return err
}

// JoinTokenConsumption mirrors components/schemas/JoinTokenConsumption.
type JoinTokenConsumption struct {
	ID               uuid.UUID  `json:"id"`
	JoinTokenID      uuid.UUID  `json:"join_token_id"`
	ConsumedByNodeID *uuid.UUID `json:"consumed_by_node_id"`
	ConsumedAt       string     `json:"consumed_at"`
	SourceIP         *string    `json:"source_ip"`
}

// JoinTokenConsumptionList is the cursor-paginated envelope of GET
// /v1/nodes/join-tokens/{id}/consumptions.
type JoinTokenConsumptionList struct {
	Data []JoinTokenConsumption `json:"data"`
	Meta struct {
		NextCursor *string `json:"next_cursor"`
	} `json:"meta"`
}

// ListJoinTokenConsumptionsParams collects the query knobs.
type ListJoinTokenConsumptionsParams struct {
	Limit  int
	Cursor string
}

// ListJoinTokenConsumptions fetches GET
// /v1/nodes/join-tokens/{id}/consumptions. Empty in Step 1 (redemption
// endpoint not yet implemented).
func (c *Client) ListJoinTokenConsumptions(ctx context.Context, id uuid.UUID, params ListJoinTokenConsumptionsParams) (JoinTokenConsumptionList, error) {
	q := url.Values{}
	if params.Limit > 0 {
		q.Set("limit", strconv.Itoa(params.Limit))
	}
	if params.Cursor != "" {
		q.Set("cursor", params.Cursor)
	}

	path := "/v1/nodes/join-tokens/" + id.String() + "/consumptions"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}

	httpReq, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return JoinTokenConsumptionList{}, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return JoinTokenConsumptionList{}, err
	}
	var out JoinTokenConsumptionList
	if err := decodeJSON(body, &out); err != nil {
		return JoinTokenConsumptionList{}, err
	}
	return out, nil
}
