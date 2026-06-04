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
	"strings"

	"github.com/google/uuid"
)

// Network mirrors the networkView projection
// internal/api/handlers/networks produces. VlanTag / MTU are *int /
// int to match the nullable / NOT-NULL columns. Status is populated
// only on the GET-by-id path; the list path leaves it nil.
type Network struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Type       string          `json:"type"`
	BridgeName string          `json:"bridge_name"`
	Managed    bool            `json:"managed"`
	Egress     string          `json:"egress"`
	VlanTag    *int            `json:"vlan_tag"`
	MTU        int             `json:"mtu"`
	Subnet     *string         `json:"subnet"`
	Gateway    *string         `json:"gateway"`
	VNI        *int            `json:"vni"`
	Config     json.RawMessage `json:"config"`
	CreatedAt  string          `json:"created_at"`
	UpdatedAt  string          `json:"updated_at"`
	Status     *NetworkStatus  `json:"status,omitempty"`
}

// NetworkStatus is the GET-by-id-only per-node reconciliation rollup
// (networkView.status). Each Nodes entry is one node that has reported
// a materialisation outcome for the network.
type NetworkStatus struct {
	Nodes []NetworkNodeStatus `json:"nodes"`
}

// NetworkNodeStatus mirrors networkNodeStatusView - one node's
// reconciliation outcome for a network. NodeName is the resolved node
// name (empty when the node was deleted but a stale status lingers);
// NodeID is the stable handle to fall back on.
type NetworkNodeStatus struct {
	NodeID               string  `json:"node_id"`
	NodeName             string  `json:"node_name"`
	ReconciliationStatus string  `json:"reconciliation_status"`
	ReconciliationError  *string `json:"reconciliation_error"`
	LastReconciledAt     *string `json:"last_reconciled_at"`
}

// NetworkList is the cursor-paginated payload of GET /v1/networks.
type NetworkList struct {
	Data []Network `json:"data"`
	Meta struct {
		NextCursor *string `json:"next_cursor"`
	} `json:"meta"`
}

// CreateNetworkParams collects the inputs for POST /v1/networks. The
// optional fields are pointers / empty-comparable so CreateNetwork can
// omit them from the JSON body and let the server apply its defaults
// (managed=false, egress=none, mtu=1500, no vlan, derived gateway).
type CreateNetworkParams struct {
	Name       string
	Type       string
	BridgeName string
	Managed    bool
	Egress     string
	Subnet     string
	Gateway    string
	VlanTag    *int
	Mtu        *int
}

// body assembles the JSON request body for POST /v1/networks, omitting
// every optional field the operator did not set so the server's
// defaults apply. Name / Type / BridgeName are always present (the CLI
// supplies Type's default and marks BridgeName required). Split out so
// the omitempty assembly is unit-testable without a live server.
func (p CreateNetworkParams) body() map[string]any {
	out := map[string]any{
		"name": p.Name,
		"type": p.Type,
	}
	if p.BridgeName != "" {
		out["bridge_name"] = p.BridgeName
	}
	if p.Managed {
		out["managed"] = true
	}
	if p.Egress != "" {
		out["egress"] = p.Egress
	}
	if p.Subnet != "" {
		out["subnet"] = p.Subnet
	}
	if p.Gateway != "" {
		out["gateway"] = p.Gateway
	}
	if p.VlanTag != nil {
		out["vlan_tag"] = *p.VlanTag
	}
	if p.Mtu != nil {
		out["mtu"] = *p.Mtu
	}
	return out
}

// ListNetworksParams collects the query knobs GET /v1/networks accepts.
// Type is an optional filter; Limit / Cursor drive cursor pagination.
type ListNetworksParams struct {
	Type   string
	Limit  int
	Cursor string
}

// ErrNetworkExists is the sentinel CreateNetwork returns when the
// server responds 409 conflict on the network-name uniqueness
// violation. Detection is status-based: the CP emits the generic
// `conflict` code, not a specialised one. Callers driving seed scripts
// use errors.Is(err, ErrNetworkExists) for re-run safety.
var ErrNetworkExists = errors.New("network name already in use")

// CreateNetwork submits POST /v1/networks. 201 returns the parsed
// Network view; 409 collapses to ErrNetworkExists; any other non-2xx
// surfaces as *APIError (including the 400 validation_failed the CP
// emits for egress=nat without managed, or a missing subnet). Admin-
// only (`network:manage`).
func (c *Client) CreateNetwork(ctx context.Context, params CreateNetworkParams) (Network, error) {
	httpReq, err := c.newRequest(ctx, http.MethodPost, "/v1/networks", params.body())
	if err != nil {
		return Network{}, err
	}
	_, respBody, err := c.do(httpReq)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict {
			return Network{}, ErrNetworkExists
		}
		return Network{}, err
	}
	var out Network
	if err := decodeJSON(respBody, &out); err != nil {
		return Network{}, err
	}
	return out, nil
}

// ListNetworks fetches GET /v1/networks and returns the parsed page.
// Pagination is the caller's responsibility (re-issue with
// NetworkList.Meta.NextCursor on Params.Cursor).
func (c *Client) ListNetworks(ctx context.Context, params ListNetworksParams) (NetworkList, error) {
	q := url.Values{}
	if params.Limit > 0 {
		q.Set("limit", strconv.Itoa(params.Limit))
	}
	if params.Cursor != "" {
		q.Set("cursor", params.Cursor)
	}
	if params.Type != "" {
		q.Set("type", params.Type)
	}

	path := "/v1/networks"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}

	httpReq, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return NetworkList{}, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return NetworkList{}, err
	}
	var out NetworkList
	if err := decodeJSON(body, &out); err != nil {
		return NetworkList{}, err
	}
	return out, nil
}

// GetNetwork fetches a single network. The CP GET-by-id route accepts
// ONLY a UUID literal (it uuid.Parse-es the path param and 400s on any
// other shape), so a non-UUID identifier is resolved to a UUID
// client-side via resolveNetworkName (list-then-match on name). The
// returned Network carries the per-node Status rollup. The raw response
// body returned alongside the decoded value is the FINAL by-UUID GET's
// body (not the list page used for name resolution), so `network get
// --output json` echoes the server's projection verbatim
// (absent-vs-null preserved); decode-only callers pass `_`.
func (c *Client) GetNetwork(ctx context.Context, identifier string) (Network, json.RawMessage, error) {
	id, err := c.resolveIdentifier(ctx, identifier)
	if err != nil {
		return Network{}, nil, err
	}
	httpReq, err := c.newRequest(ctx, http.MethodGet, "/v1/networks/"+id, nil)
	if err != nil {
		return Network{}, nil, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return Network{}, nil, err
	}
	var out Network
	if err := decodeJSON(body, &out); err != nil {
		return Network{}, nil, err
	}
	return out, json.RawMessage(body), nil
}

// DeleteNetwork submits DELETE /v1/networks/{id}. As with GetNetwork
// the route accepts only a UUID, so a name identifier is resolved
// client-side first. 204 returns nil; 409 collapses to *ErrNetworkBlocked
// carrying the parsed blocking_resources (key: "vm_nics"); any other
// non-2xx surfaces as *APIError. Admin-only.
func (c *Client) DeleteNetwork(ctx context.Context, identifier string) error {
	id, err := c.resolveIdentifier(ctx, identifier)
	if err != nil {
		return err
	}
	httpReq, err := c.newRequest(ctx, http.MethodDelete, "/v1/networks/"+id, nil)
	if err != nil {
		return err
	}
	_, _, err = c.do(httpReq)
	if err == nil {
		return nil
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict {
		if blocked := decodeNetworkBlockingResources(apiErr); blocked != nil {
			return blocked
		}
	}
	return err
}

// resolveIdentifier returns identifier unchanged when it is already a
// UUID literal, otherwise treats it as a network name and resolves it
// to a UUID via resolveNetworkName. The CP GET/DELETE routes accept
// only UUIDs, so this is the single name-resolution seam shared by
// GetNetwork and DeleteNetwork.
func (c *Client) resolveIdentifier(ctx context.Context, identifier string) (string, error) {
	if uuid.Validate(identifier) == nil {
		return identifier, nil
	}
	return c.resolveNetworkName(ctx, identifier)
}

// resolveNetworkName pages GET /v1/networks looking for a name match
// and returns its UUID. Matching is case-insensitive to mirror the
// store's lowercased name guard and the server-side NetworkByName lookup
// (used by `vm create --network`), so name resolution stays consistent
// across CLI verbs. Networks carry a globally-unique name
// (uq_networks_name), so at most one row matches. Returns
// ErrNetworkNotFound when no row matches across all pages.
func (c *Client) resolveNetworkName(ctx context.Context, name string) (string, error) {
	cursor := ""
	for {
		page, err := c.ListNetworks(ctx, ListNetworksParams{Limit: 200, Cursor: cursor})
		if err != nil {
			return "", err
		}
		for _, n := range page.Data {
			if strings.EqualFold(n.Name, name) {
				return n.ID, nil
			}
		}
		if page.Meta.NextCursor == nil || *page.Meta.NextCursor == "" {
			return "", fmt.Errorf("%w: %q", ErrNetworkNotFound, name)
		}
		cursor = *page.Meta.NextCursor
	}
}

// ErrNetworkNotFound is returned by the name-resolution path when no
// network carries the requested name. Callers surface it as a clean
// "no such network" rather than leaking the list-then-match mechanics.
var ErrNetworkNotFound = errors.New("network not found")

// ErrNetworkBlocked is the typed error DeleteNetwork returns when the
// CP refuses the delete because the network still has dependent
// vm_nics. Mirrors response.BlockingResourcesError on the wire:
//
//   - Code is the server-set `error.code`.
//   - Resources maps dependent-resource-type → reference count. The
//     only known key is "vm_nics".
type ErrNetworkBlocked struct {
	Code      string
	Resources map[string]int64
}

// Error renders the operator-facing message listing resource types
// and counts.
func (e *ErrNetworkBlocked) Error() string {
	return fmt.Sprintf("network delete blocked: %s: %v", e.Code, e.Resources)
}

// decodeNetworkBlockingResources lifts the `details.blocking_resources`
// map off an APIError into the typed ErrNetworkBlocked shape. Returns
// nil when the details payload is absent or malformed so the caller
// falls back to the raw *APIError. Parallel of
// decodePoolBlockingResources; kept separate to keep the typed-error
// shape per-domain.
func decodeNetworkBlockingResources(apiErr *APIError) *ErrNetworkBlocked {
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
	return &ErrNetworkBlocked{
		Code:      apiErr.Code,
		Resources: resources,
	}
}
