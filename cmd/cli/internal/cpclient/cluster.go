// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cpclient

import (
	"context"
	"errors"
	"net/http"

	"github.com/otherix/otherix/internal/api/response"
)

// ClusterDefaultPool mirrors the response shape of the
// /v1/cluster/default-pool endpoints. The `Name` value is the canonical
// case the server holds for the pool name (case-insensitive matching
// on the way in; canonical case on the way out).
type ClusterDefaultPool struct {
	Name string `json:"name"`
}

// GetClusterDefaultPool fetches GET /v1/cluster/default-pool. Returns
// (nil, nil) when the cluster default-pool reference is unset
// (envelope code `default_pool_not_set`, 404). Other non-2xx
// responses surface as *APIError so callers can branch on the wire
// code.
func (c *Client) GetClusterDefaultPool(ctx context.Context) (*ClusterDefaultPool, error) {
	httpReq, err := c.newRequest(ctx, http.MethodGet, "/v1/cluster/default-pool", nil)
	if err != nil {
		return nil, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Code == string(response.CodeDefaultPoolNotSet) {
			return nil, nil
		}
		return nil, err
	}
	var out ClusterDefaultPool
	if err := decodeJSON(body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetClusterDefaultPool submits PUT /v1/cluster/default-pool. The
// server validates the supplied name resolves to at least one existing
// pool instance — unknown names surface as *APIError carrying
// `pool_not_found`.
func (c *Client) SetClusterDefaultPool(ctx context.Context, name string) (ClusterDefaultPool, error) {
	httpReq, err := c.newRequest(ctx, http.MethodPut, "/v1/cluster/default-pool", ClusterDefaultPool{Name: name})
	if err != nil {
		return ClusterDefaultPool{}, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return ClusterDefaultPool{}, err
	}
	var out ClusterDefaultPool
	if err := decodeJSON(body, &out); err != nil {
		return ClusterDefaultPool{}, err
	}
	return out, nil
}

// ClearClusterDefaultPool submits DELETE /v1/cluster/default-pool.
// Idempotent on the wire — clearing an already-unset reference
// returns 204.
func (c *Client) ClearClusterDefaultPool(ctx context.Context) error {
	httpReq, err := c.newRequest(ctx, http.MethodDelete, "/v1/cluster/default-pool", nil)
	if err != nil {
		return err
	}
	_, _, err = c.do(httpReq)
	return err
}
