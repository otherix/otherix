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

	"github.com/google/uuid"
)

// Pool mirrors the response shape internal/api/handlers/storagepools
// produces for the flat per-instance projection. The per-row
// `is_default` is gone; `IsClusterDefault` is the derived flag for
// "this instance's pool name is the cluster default". `Node` carries
// the owning node's name.
type Pool struct {
	ID                      uuid.UUID          `json:"id"`
	Node                    string             `json:"node"`
	NodeStatus              *string            `json:"node_status"`
	Name                    string             `json:"name"`
	Type                    string             `json:"type"`
	Path                    string             `json:"path"`
	CapacityBytes           *int64             `json:"capacity_bytes"`
	AvailableBytes          *int64             `json:"available_bytes"`
	AvailableBytesEffective *int64             `json:"available_bytes_effective"`
	ReportedAt              *string            `json:"reported_at"`
	IsClusterDefault        bool               `json:"is_cluster_default"`
	Config                  json.RawMessage    `json:"config"`
	DiskPressure            *PressureCondition `json:"disk_pressure"`
	ReconciliationStatus    string             `json:"reconciliation_status"`
	LastReconciledAt        *string            `json:"last_reconciled_at"`
	ReconciliationError     *string            `json:"reconciliation_error"`
	Images                  []PoolImage        `json:"images"`
	CreatedAt               string             `json:"created_at"`
	UpdatedAt               string             `json:"updated_at"`
}

// PoolImage is one cached source image the server has downloaded into a
// storage pool. It replaces the old template-cache view: VMs now boot
// directly from an image URL, and the cached copies surface here so
// operators can see what is already local to a pool.
type PoolImage struct {
	Name             string `json:"name"`
	SHA256           string `json:"sha256"`
	SizeBytes        int64  `json:"size_bytes"`
	VirtualSizeBytes int64  `json:"virtual_size_bytes"`
	Format           string `json:"format"`
	ImportedAt       string `json:"imported_at"`
}

// PoolList is the cursor-paginated payload of GET /v1/storage-pools.
type PoolList struct {
	Data []Pool `json:"data"`
	Meta struct {
		NextCursor *string `json:"next_cursor"`
	} `json:"meta"`
}

// PoolConceptView is the aggregated cluster-wide projection returned
// by GET /v1/storage-pools/{name}. The `Name` value is the canonical
// case the server holds; `Instances` carries every per-node row.
// IsClusterDefault is true when the name matches cluster_settings.
// default_pool_name.
type PoolConceptView struct {
	Name             string `json:"name"`
	Type             string `json:"type"`
	IsClusterDefault bool   `json:"is_cluster_default"`
	Instances        []Pool `json:"instances"`
}

// ListPoolsParams collects the query knobs the GET /v1/storage-pools
// endpoint accepts. Node / Type are optional filters; Limit / Cursor
// drive cursor pagination.
type ListPoolsParams struct {
	Limit  int
	Cursor string
	Node   string
	Type   string
}

// ListPools fetches GET /v1/storage-pools and returns the parsed page.
// Pagination is the caller's responsibility (re-issue with
// PoolList.Meta.NextCursor on Params.Cursor).
func (c *Client) ListPools(ctx context.Context, params ListPoolsParams) (PoolList, error) {
	q := url.Values{}
	if params.Limit > 0 {
		q.Set("limit", strconv.Itoa(params.Limit))
	}
	if params.Cursor != "" {
		q.Set("cursor", params.Cursor)
	}
	if params.Node != "" {
		q.Set("node", params.Node)
	}
	if params.Type != "" {
		q.Set("type", params.Type)
	}

	path := "/v1/storage-pools"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}

	httpReq, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return PoolList{}, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return PoolList{}, err
	}
	var out PoolList
	if err := decodeJSON(body, &out); err != nil {
		return PoolList{}, err
	}
	return out, nil
}

// GetPoolByID fetches GET /v1/storage-pools/{id} with a UUID literal and
// returns the flat per-instance Pool view. Use this when targeting a
// specific node's pool row (the equivalent of addressing a Kubernetes
// PersistentVolume by name). The raw response body is returned alongside
// the decoded value so `pool get --output json` echoes the server's
// projection verbatim (absent-vs-null preserved); decode-only callers
// pass `_`.
func (c *Client) GetPoolByID(ctx context.Context, id uuid.UUID) (Pool, json.RawMessage, error) {
	httpReq, err := c.newRequest(ctx, http.MethodGet, "/v1/storage-pools/"+id.String(), nil)
	if err != nil {
		return Pool{}, nil, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return Pool{}, nil, err
	}
	var out Pool
	if err := decodeJSON(body, &out); err != nil {
		return Pool{}, nil, err
	}
	return out, json.RawMessage(body), nil
}

// GetPoolByName fetches GET /v1/storage-pools/{name} and returns the
// aggregated PoolConceptView. Use this when speaking at the cluster
// name level (the equivalent of addressing a Kubernetes StorageClass);
// the response carries every per-node instance plus the cluster
// default-pool flag. The raw response body is returned alongside the
// decoded value so `pool get --output json` echoes the server's
// aggregated projection verbatim (absent-vs-null preserved); decode-only
// callers pass `_`.
func (c *Client) GetPoolByName(ctx context.Context, name string) (PoolConceptView, json.RawMessage, error) {
	httpReq, err := c.newRequest(ctx, http.MethodGet, "/v1/storage-pools/"+url.PathEscape(name), nil)
	if err != nil {
		return PoolConceptView{}, nil, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return PoolConceptView{}, nil, err
	}
	var out PoolConceptView
	if err := decodeJSON(body, &out); err != nil {
		return PoolConceptView{}, nil, err
	}
	return out, json.RawMessage(body), nil
}

// CreatePoolRequest mirrors components/schemas/StoragePoolCreate in
// api/openapi/control-plane.yaml. The `node` field accepts a node name
// (UUID literals are rejected with 400 validation_failed). `config` is
// optional — omitted bodies land as the canonical empty object on the
// server.
type CreatePoolRequest struct {
	Node   string                 `json:"node"`
	Name   string                 `json:"name"`
	Type   string                 `json:"type"`
	Path   string                 `json:"path"`
	Config map[string]interface{} `json:"config,omitempty"`
}

// ErrPoolExists is the sentinel returned by CreatePool when the server
// responds 409 conflict on the (name, node) uniqueness violation.
// Detection is purely status-based: the CP emits the generic `conflict`
// code on uq_storage_pools_name, not a specialised `pool_exists` code.
// The CLI uses `errors.Is(err, ErrPoolExists)` to drive idempotent
// re-runs of seed scripts that may hit an already-registered pool.
var ErrPoolExists = errors.New("storage pool already exists on the target node")

// CreatePool submits POST /v1/storage-pools. 201 returns the parsed
// Pool view; 409 collapses to ErrPoolExists; any other non-2xx surfaces
// as *APIError. Admin-only (`storage_pool:manage`); the CP returns
// 403 permission_denied for other roles.
func (c *Client) CreatePool(ctx context.Context, req CreatePoolRequest) (Pool, error) {
	httpReq, err := c.newRequest(ctx, http.MethodPost, "/v1/storage-pools", req)
	if err != nil {
		return Pool{}, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict {
			return Pool{}, ErrPoolExists
		}
		return Pool{}, err
	}
	var out Pool
	if err := decodeJSON(body, &out); err != nil {
		return Pool{}, err
	}
	return out, nil
}

// ErrPoolBlocked is the typed error DeletePool returns when the CP
// refuses the delete because the pool still has dependents. Mirrors
// response.BlockingResourcesError on the wire side:
//
//   - Code is the server-set `error.code` — generic `conflict` for the
//     vm_disks blocking branch.
//   - Resources maps dependent-resource-type → reference count. Known
//     key: "vm_disks".
type ErrPoolBlocked struct {
	Code      string
	Resources map[string]int64
}

// Error renders the operator-facing message, listing resource types
// and counts sorted by key for stable output.
func (e *ErrPoolBlocked) Error() string {
	return fmt.Sprintf("storage pool delete blocked: %s: %v", e.Code, e.Resources)
}

// DeletePool submits DELETE /v1/storage-pools/{identifier}. The server
// accepts either a UUID literal or a pool name — single-instance pools
// resolve cleanly; multi-instance names return 400 multiple_instances
// and the caller must address by UUID instead. 204 returns nil; 409
// collapses to *ErrPoolBlocked carrying the parsed blocking_resources
// map; any other non-2xx (404 / 5xx) surfaces as *APIError. Admin-only.
func (c *Client) DeletePool(ctx context.Context, identifier string) error {
	httpReq, err := c.newRequest(ctx, http.MethodDelete, "/v1/storage-pools/"+url.PathEscape(identifier), nil)
	if err != nil {
		return err
	}
	_, _, err = c.do(httpReq)
	if err == nil {
		return nil
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict {
		if blocked := decodePoolBlockingResources(apiErr); blocked != nil {
			return blocked
		}
	}
	return err
}

// decodePoolBlockingResources lifts the `details.blocking_resources`
// map off an APIError into the typed ErrPoolBlocked shape. Returns nil
// when the details payload is absent or malformed — the caller falls
// back to the raw *APIError so the operator at least sees the status
// code. The typed-error shape is kept per-domain rather than shared.
func decodePoolBlockingResources(apiErr *APIError) *ErrPoolBlocked {
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
	return &ErrPoolBlocked{
		Code:      apiErr.Code,
		Resources: resources,
	}
}
