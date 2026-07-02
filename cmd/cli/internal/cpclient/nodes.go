// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"github.com/google/uuid"
)

// Node carries both the full admin/operator projection and the
// reduced developer/viewer summary in a single struct: every
// admin-only field uses a pointer / json.RawMessage so a summary-view
// JSON response decodes cleanly with those fields left zero/nil. The
// always-present fields (ID, Name, Architecture, Status, Labels,
// CreatedAt) match between the two server-side shapes.
//
// Nodes have no owner column and no dual-shape per role — the
// per-role split happens at the server's projection edge (nodeView
// vs nodeSummaryView). This client merges both shapes so the CLI can
// render whichever fields the wire envelope carries.
type Node struct {
	ID           uuid.UUID         `json:"id"`
	Name         string            `json:"name"`
	Architecture string            `json:"architecture"`
	Roles        []string          `json:"roles"`
	Status       string            `json:"status"`
	CordonedAt   *string           `json:"cordoned_at,omitempty"`
	Labels       map[string]string `json:"labels"`
	CreatedAt    string            `json:"created_at"`

	// Admin / operator full-view fields. Optional pointers / raw
	// values: nil in the developer/viewer summary projection.
	UpdatedAt                *string            `json:"updated_at,omitempty"`
	AdvertisedEndpoint       *string            `json:"advertised_endpoint,omitempty"`
	Migration                *MigrationCap      `json:"migration,omitempty"`
	CPUCoresTotal            *int32             `json:"cpu_cores_total,omitempty"`
	CPUCoresAvailable        *int32             `json:"cpu_cores_available,omitempty"`
	CPUCoresEffective        *int32             `json:"cpu_cores_effective,omitempty"`
	CPUModel                 *string            `json:"cpu_model,omitempty"`
	CPUFlags                 []string           `json:"cpu_flags,omitempty"`
	MemoryTotalMiB           *int64             `json:"memory_total_mib,omitempty"`
	MemoryAvailableMiB       *int64             `json:"memory_available_mib,omitempty"`
	MemoryEffectiveMiB       *int64             `json:"memory_effective_mib,omitempty"`
	Hugepages2MiB            *int32             `json:"hugepages_2mib_total,omitempty"`
	Hugepages1GiB            *int32             `json:"hugepages_1gib_total,omitempty"`
	KernelVersion            *string            `json:"kernel_version,omitempty"`
	QEMUVersion              *string            `json:"qemu_version,omitempty"`
	NumaTopology             json.RawMessage    `json:"numa_topology,omitempty"`
	Capabilities             json.RawMessage    `json:"capabilities,omitempty"`
	LastHeartbeatAt          *string            `json:"last_heartbeat_at,omitempty"`
	AgentVersion             *string            `json:"agent_version,omitempty"`
	MemoryPressure           *PressureCondition `json:"memory_pressure,omitempty"`
	SystemDiskTotalBytes     *int64             `json:"system_disk_total_bytes,omitempty"`
	SystemDiskAvailableBytes *int64             `json:"system_disk_available_bytes,omitempty"`
	SystemDiskPressure       *PressureCondition `json:"system_disk_pressure,omitempty"`
	WireGuard                *NodeWireguard     `json:"wireguard,omitempty"`
}

// NodeWireguard mirrors the server's NodeWireguard schema: the node's WG
// underlay fabric identity plus its mesh peers. nil for callers/roles that do
// not receive it (developer/viewer, or a node that has not reported WG yet).
type NodeWireguard struct {
	OverlayIP  string              `json:"overlay_ip"`
	PublicKey  string              `json:"public_key"`
	ListenPort int32               `json:"listen_port"`
	Endpoint   string              `json:"endpoint"`
	Status     string              `json:"reconciliation_status"`
	Error      *string             `json:"reconciliation_error"`
	Peers      []NodeWireguardPeer `json:"peers"`
}

// NodeWireguardPeer is one other agent in the node's mesh. NodeName is nil when
// the peer's node row was deleted; render falls back to NodeID.
type NodeWireguardPeer struct {
	NodeID      string  `json:"node_id"`
	NodeName    *string `json:"node_name"`
	OverlayIP   string  `json:"overlay_ip"`
	Established bool    `json:"established"`
}

// PressureCondition mirrors the wire schema's MemoryPressureCondition
// / SystemDiskPressureCondition / DiskPressureCondition. All three
// pressure types share the same wire shape — a single struct decodes
// any of them. NULL when pressure is not active; `omitempty` keeps
// an empty payload off the JSON wire entirely.
type PressureCondition struct {
	Since            string `json:"since"`
	ConsecutiveCount int32  `json:"consecutive_count"`
}

// MemoryPressure is a legacy alias retained for callers that imported
// the earlier name. New code should use PressureCondition directly.
type MemoryPressure = PressureCondition

// MigrationCap mirrors the wire schema's MigrationCapability. Surfaced
// to admin / operator only; developer / viewer see Node.Migration ==
// nil.
type MigrationCap struct {
	Host           string `json:"host"`
	PortRangeStart int32  `json:"port_range_start"`
	PortRangeEnd   int32  `json:"port_range_end"`
}

// NodeList is the cursor-paginated envelope of GET /v1/nodes. Per-row
// shape per the caller's role; the merged Node struct above handles
// both.
type NodeList struct {
	Data []Node `json:"data"`
	Meta struct {
		NextCursor *string `json:"next_cursor"`
	} `json:"meta"`
}

// ListNodesParams collects the query knobs the list endpoint accepts.
// Architecture / Status are server-validated filters; Limit / Cursor
// drive cursor pagination.
type ListNodesParams struct {
	Limit        int
	Cursor       string
	Architecture string
	Status       string
}

// ListNodes fetches GET /v1/nodes with the supplied query filters.
// Pagination is the caller's responsibility (re-issue with
// NodeList.Meta.NextCursor on Params.Cursor).
func (c *Client) ListNodes(ctx context.Context, params ListNodesParams) (NodeList, error) {
	q := url.Values{}
	if params.Limit > 0 {
		q.Set("limit", strconv.Itoa(params.Limit))
	}
	if params.Cursor != "" {
		q.Set("cursor", params.Cursor)
	}
	if params.Architecture != "" {
		q.Set("architecture", params.Architecture)
	}
	if params.Status != "" {
		q.Set("status", params.Status)
	}

	path := "/v1/nodes"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}

	httpReq, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return NodeList{}, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return NodeList{}, err
	}
	var out NodeList
	if err := decodeJSON(body, &out); err != nil {
		return NodeList{}, err
	}
	return out, nil
}

// DrainNode submits POST /v1/nodes/{node}/drain - async per the
// AsyncTaskAccepted pattern, returning the 202 task. A positive
// timeoutSeconds overrides the cluster-default drain budget for this drain;
// a non-positive value omits the body field so the server applies the
// cluster default. Non-2xx surfaces as *APIError (e.g. 409 conflict when the
// node is not in a drainable state).
func (c *Client) DrainNode(ctx context.Context, node string, timeoutSeconds int32) (TaskAccepted, error) {
	var body any
	if timeoutSeconds > 0 {
		body = struct {
			TimeoutSeconds int32 `json:"timeout_seconds"`
		}{TimeoutSeconds: timeoutSeconds}
	}
	httpReq, err := c.newRequest(ctx, http.MethodPost, "/v1/nodes/"+url.PathEscape(node)+"/drain", body)
	if err != nil {
		return TaskAccepted{}, err
	}
	_, raw, err := c.do(httpReq)
	if err != nil {
		return TaskAccepted{}, err
	}
	var task TaskAccepted
	if err := decodeJSON(raw, &task); err != nil {
		return TaskAccepted{}, err
	}
	return task, nil
}

// CordonNode submits POST /v1/nodes/{node}/cordon - sync (200), idempotent.
// The CLI discards the returned node view; the action's effect is the cordon.
// Non-2xx surfaces as *APIError (e.g. 409 conflict for a non-cordonable state).
func (c *Client) CordonNode(ctx context.Context, node string) error {
	return c.nodeAction(ctx, node, "cordon")
}

// UncordonNode submits POST /v1/nodes/{node}/uncordon - sync (200),
// idempotent. The documented reverse of a completed drain. Non-2xx surfaces
// as *APIError (e.g. 409 conflict for a non-uncordonable state).
func (c *Client) UncordonNode(ctx context.Context, node string) error {
	return c.nodeAction(ctx, node, "uncordon")
}

// DeleteNode submits DELETE /v1/nodes/{node}, adding ?force=true when force is
// set. Sync 204. Non-2xx surfaces as *APIError - notably 409 conflict with
// blocking_resources when the node still hosts VMs / active migrations and
// force was not given.
func (c *Client) DeleteNode(ctx context.Context, node string, force bool) error {
	path := "/v1/nodes/" + url.PathEscape(node)
	if force {
		path += "?force=true"
	}
	httpReq, err := c.newRequest(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	if _, _, err := c.do(httpReq); err != nil {
		return err
	}
	return nil
}

// ReadmitNode submits POST /v1/nodes/{node}/readmit - sync (200), idempotent.
// Returns a node stuck in the terminal gone status to pending. Non-2xx surfaces
// as *APIError (e.g. 409 conflict for a node that is not gone).
func (c *Client) ReadmitNode(ctx context.Context, node string) error {
	return c.nodeAction(ctx, node, "readmit")
}

// nodeAction POSTs a sync node maintenance verb (cordon / uncordon) and
// discards the 200 body. Any non-2xx surfaces as *APIError via do.
func (c *Client) nodeAction(ctx context.Context, node, action string) error {
	httpReq, err := c.newRequest(ctx, http.MethodPost, "/v1/nodes/"+url.PathEscape(node)+"/"+action, nil)
	if err != nil {
		return err
	}
	if _, _, err := c.do(httpReq); err != nil {
		return err
	}
	return nil
}

// GetNode fetches GET /v1/nodes/{identifier}. The server resolves
// either a UUID literal or a node name; the CLI forwards the raw
// string verbatim. The per-role projection (full vs summary) is
// surfaced through the Node struct's optional fields — admin / operator
// callers see Migration / CPUCoresTotal / etc. populated; others see
// them nil. The raw response body is returned alongside the decoded
// value so `get --output json` can echo the server's projection
// verbatim (absent-vs-null preserved); decode-only callers pass `_`.
func (c *Client) GetNode(ctx context.Context, identifier string) (Node, json.RawMessage, error) {
	httpReq, err := c.newRequest(ctx, http.MethodGet, "/v1/nodes/"+url.PathEscape(identifier), nil)
	if err != nil {
		return Node{}, nil, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return Node{}, nil, err
	}
	var out Node
	if err := decodeJSON(body, &out); err != nil {
		return Node{}, nil, err
	}
	return out, json.RawMessage(body), nil
}
