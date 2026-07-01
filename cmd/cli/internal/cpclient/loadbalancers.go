// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
)

// LoadBalancer mirrors the LoadBalancer projection the CP
// /v1/loadbalancers surface produces. A load balancer is a named L4
// front for the VMs whose labels match Selector; Port is the guest TCP
// port ingress connections target.
type LoadBalancer struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	OwnerID   string            `json:"owner_id"`
	Port      int32             `json:"port"`
	Selector  map[string]string `json:"selector"`
	CreatedAt string            `json:"created_at"`
	UpdatedAt string            `json:"updated_at"`
}

// LoadBalancerList is the cursor-paginated payload of
// GET /v1/loadbalancers.
type LoadBalancerList struct {
	Data []LoadBalancer `json:"data"`
	Meta struct {
		NextCursor *string `json:"next_cursor"`
	} `json:"meta"`
}

// CreateLoadBalancerParams collects the inputs for POST
// /v1/loadbalancers. All three fields are required by the server
// (LoadBalancerCreate: name, port, selector).
type CreateLoadBalancerParams struct {
	Name     string
	Port     int32
	Selector map[string]string
}

// body assembles the JSON request body for POST /v1/loadbalancers.
// Split out so the assembly is unit-testable without a live server.
func (p CreateLoadBalancerParams) body() map[string]any {
	return map[string]any{
		"name":     p.Name,
		"port":     p.Port,
		"selector": p.Selector,
	}
}

// UpdateLoadBalancerParams collects the optional fields for PATCH
// /v1/loadbalancers/{name}. Every field is a pointer so an unset field
// is omitted from the request body and the server leaves it unchanged
// (LoadBalancerUpdate: all fields optional).
type UpdateLoadBalancerParams struct {
	Name     *string
	Port     *int32
	Selector map[string]string
}

// body assembles the PATCH body, omitting every field the caller did
// not set so the server leaves it untouched.
func (p UpdateLoadBalancerParams) body() map[string]any {
	out := map[string]any{}
	if p.Name != nil {
		out["name"] = *p.Name
	}
	if p.Port != nil {
		out["port"] = *p.Port
	}
	if p.Selector != nil {
		out["selector"] = p.Selector
	}
	return out
}

// ListLoadBalancersParams collects the query knobs GET
// /v1/loadbalancers accepts: Limit / Cursor drive cursor pagination.
type ListLoadBalancersParams struct {
	Limit  int
	Cursor string
}

// CreateLoadBalancer submits POST /v1/loadbalancers. 201 returns the
// parsed LoadBalancer view; any non-2xx surfaces as *APIError.
func (c *Client) CreateLoadBalancer(ctx context.Context, params CreateLoadBalancerParams) (LoadBalancer, error) {
	httpReq, err := c.newRequest(ctx, http.MethodPost, "/v1/loadbalancers", params.body())
	if err != nil {
		return LoadBalancer{}, err
	}
	_, respBody, err := c.do(httpReq)
	if err != nil {
		return LoadBalancer{}, err
	}
	var out LoadBalancer
	if err := decodeJSON(respBody, &out); err != nil {
		return LoadBalancer{}, err
	}
	return out, nil
}

// ListLoadBalancers fetches GET /v1/loadbalancers and returns the
// parsed page. Pagination is the caller's responsibility (re-issue with
// LoadBalancerList.Meta.NextCursor on Params.Cursor).
func (c *Client) ListLoadBalancers(ctx context.Context, params ListLoadBalancersParams) (LoadBalancerList, error) {
	q := url.Values{}
	if params.Limit > 0 {
		q.Set("limit", strconv.Itoa(params.Limit))
	}
	if params.Cursor != "" {
		q.Set("cursor", params.Cursor)
	}

	path := "/v1/loadbalancers"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}

	httpReq, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return LoadBalancerList{}, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return LoadBalancerList{}, err
	}
	var out LoadBalancerList
	if err := decodeJSON(body, &out); err != nil {
		return LoadBalancerList{}, err
	}
	return out, nil
}

// GetLoadBalancer fetches a single load balancer. The CP GET-by-id
// route addresses the load balancer by NAME (the `{id}` path param is
// the name, not a UUID), so the identifier is passed through verbatim -
// no client-side name->uuid resolution (unlike networks). The raw
// response body returned alongside the decoded value is the server's
// projection verbatim so `lb get --output json` preserves absent-vs-null;
// decode-only callers pass `_`.
func (c *Client) GetLoadBalancer(ctx context.Context, name string) (LoadBalancer, json.RawMessage, error) {
	httpReq, err := c.newRequest(ctx, http.MethodGet, "/v1/loadbalancers/"+url.PathEscape(name), nil)
	if err != nil {
		return LoadBalancer{}, nil, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return LoadBalancer{}, nil, err
	}
	var out LoadBalancer
	if err := decodeJSON(body, &out); err != nil {
		return LoadBalancer{}, nil, err
	}
	return out, json.RawMessage(body), nil
}

// UpdateLoadBalancer submits PATCH /v1/loadbalancers/{name}. Only the
// fields set on params are sent; the CP applies the changes and returns
// the updated view. Non-2xx surfaces as *APIError.
func (c *Client) UpdateLoadBalancer(ctx context.Context, name string, params UpdateLoadBalancerParams) (LoadBalancer, error) {
	httpReq, err := c.newRequest(ctx, http.MethodPatch, "/v1/loadbalancers/"+url.PathEscape(name), params.body())
	if err != nil {
		return LoadBalancer{}, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return LoadBalancer{}, err
	}
	var out LoadBalancer
	if err := decodeJSON(body, &out); err != nil {
		return LoadBalancer{}, err
	}
	return out, nil
}

// DeleteLoadBalancer submits DELETE /v1/loadbalancers/{name}. The route
// addresses by name, so the identifier is passed through verbatim. 204
// returns nil; any non-2xx surfaces as *APIError.
func (c *Client) DeleteLoadBalancer(ctx context.Context, name string) error {
	httpReq, err := c.newRequest(ctx, http.MethodDelete, "/v1/loadbalancers/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	_, _, err = c.do(httpReq)
	return err
}
