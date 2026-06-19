// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// ArtifactPool mirrors the CP /v1/artifact-pools response shape
// (artifactpools.artifactPoolView). ReplicationFactor is kept as a raw JSON
// value because the wire form is an integer OR the string "all".
type ArtifactPool struct {
	ID                string          `json:"id"`
	Name              string          `json:"name"`
	ReplicationFactor json.RawMessage `json:"replication_factor"`
	Membership        struct {
		AllNodes bool     `json:"all_nodes"`
		Nodes    []string `json:"nodes"`
	} `json:"membership"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// ArtifactPoolCreateBody is the POST /v1/artifact-pools request body.
// ReplicationFactor is a raw JSON value (an integer or the string "all");
// Membership is the optional {all_nodes, nodes} object.
type ArtifactPoolCreateBody struct {
	Name              string          `json:"name"`
	ReplicationFactor json.RawMessage `json:"replication_factor"`
	Membership        any             `json:"membership,omitempty"`
}

// ArtifactPoolUpdateBody is the PATCH /v1/artifact-pools/{id} request body.
// Both fields are optional pointers so an unchanged field is omitted from the
// JSON: only the supplied flags reach the server. ReplicationFactor is a raw
// JSON value (an integer or the string "all"); Membership is the
// {all_nodes, nodes} object.
type ArtifactPoolUpdateBody struct {
	ReplicationFactor *json.RawMessage `json:"replication_factor,omitempty"`
	Membership        any              `json:"membership,omitempty"`
}

type artifactPoolList struct {
	Data []ArtifactPool `json:"data"`
	Meta struct {
		NextCursor *string `json:"next_cursor"`
	} `json:"meta"`
}

// CreateArtifactPool submits POST /v1/artifact-pools. Admin-only
// (storage_pool:manage). Non-2xx surfaces as *APIError.
func (c *Client) CreateArtifactPool(ctx context.Context, body ArtifactPoolCreateBody) (ArtifactPool, error) {
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/artifact-pools", body)
	if err != nil {
		return ArtifactPool{}, err
	}
	_, raw, err := c.do(req)
	if err != nil {
		return ArtifactPool{}, err
	}
	var out ArtifactPool
	return out, decodeJSON(raw, &out)
}

// GetArtifactPool submits GET /v1/artifact-pools/{identifier}. The CP route
// resolves a UUID OR a name server-side, so the identifier is passed through
// verbatim (no client-side name resolution).
func (c *Client) GetArtifactPool(ctx context.Context, identifier string) (ArtifactPool, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/artifact-pools/"+url.PathEscape(identifier), nil)
	if err != nil {
		return ArtifactPool{}, err
	}
	_, raw, err := c.do(req)
	if err != nil {
		return ArtifactPool{}, err
	}
	var out ArtifactPool
	return out, decodeJSON(raw, &out)
}

// UpdateArtifactPool submits PATCH /v1/artifact-pools/{identifier} (UUID or
// name). Only the fields set on body are sent; the CP applies the changes and
// returns the updated pool. Admin-only (storage_pool:manage). Non-2xx surfaces
// as *APIError.
func (c *Client) UpdateArtifactPool(ctx context.Context, identifier string, body ArtifactPoolUpdateBody) (ArtifactPool, error) {
	req, err := c.newRequest(ctx, http.MethodPatch, "/v1/artifact-pools/"+url.PathEscape(identifier), body)
	if err != nil {
		return ArtifactPool{}, err
	}
	_, raw, err := c.do(req)
	if err != nil {
		return ArtifactPool{}, err
	}
	var out ArtifactPool
	return out, decodeJSON(raw, &out)
}

// ListArtifactPools submits GET /v1/artifact-pools (one cursor page). The
// returned string is the next-page cursor (empty on the last page).
func (c *Client) ListArtifactPools(ctx context.Context, limit int, cursor string) ([]ArtifactPool, string, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	path := "/v1/artifact-pools"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, "", err
	}
	_, raw, err := c.do(req)
	if err != nil {
		return nil, "", err
	}
	var out artifactPoolList
	if err := decodeJSON(raw, &out); err != nil {
		return nil, "", err
	}
	next := ""
	if out.Meta.NextCursor != nil {
		next = *out.Meta.NextCursor
	}
	return out.Data, next, nil
}

// DeleteArtifactPool submits DELETE /v1/artifact-pools/{identifier} (UUID or
// name). 204 returns nil; 409 collapses to *ErrArtifactPoolBlocked carrying the
// parsed blocking_resources (key: "snapshots"); any other non-2xx surfaces as
// *APIError. Admin-only (storage_pool:manage).
func (c *Client) DeleteArtifactPool(ctx context.Context, identifier string) error {
	req, err := c.newRequest(ctx, http.MethodDelete, "/v1/artifact-pools/"+url.PathEscape(identifier), nil)
	if err != nil {
		return err
	}
	_, _, err = c.do(req)
	if err == nil {
		return nil
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict {
		if blocked := decodeArtifactPoolBlockingResources(apiErr); blocked != nil {
			return blocked
		}
	}
	return err
}

// ErrArtifactPoolBlocked is the typed error DeleteArtifactPool returns when the
// CP refuses the delete because the pool still backs snapshots. Mirrors
// response.BlockingResourcesError on the wire (parallel of ErrNetworkBlocked):
//
//   - Code is the server-set `error.code`.
//   - Resources maps dependent-resource-type -> reference count. The only
//     known key is "snapshots".
type ErrArtifactPoolBlocked struct {
	Code      string
	Resources map[string]int64
}

// Error renders the operator-facing message listing resource types and counts.
func (e *ErrArtifactPoolBlocked) Error() string {
	return fmt.Sprintf("artifact pool delete blocked: %s: %v", e.Code, e.Resources)
}

// decodeArtifactPoolBlockingResources lifts the `details.blocking_resources`
// map off an APIError into the typed ErrArtifactPoolBlocked shape. Returns nil
// when the details payload is absent or malformed so the caller falls back to
// the raw *APIError. Parallel of decodeNetworkBlockingResources.
func decodeArtifactPoolBlockingResources(apiErr *APIError) *ErrArtifactPoolBlocked {
	if apiErr.Details == nil {
		return nil
	}
	raw, ok := apiErr.Details["blocking_resources"]
	if !ok {
		return nil
	}
	mapAny, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	resources := make(map[string]int64, len(mapAny))
	for k, v := range mapAny {
		switch n := v.(type) {
		case float64:
			resources[k] = int64(n)
		case int64:
			resources[k] = n
		}
	}
	if len(resources) == 0 {
		return nil
	}
	return &ErrArtifactPoolBlocked{
		Code:      apiErr.Code,
		Resources: resources,
	}
}
