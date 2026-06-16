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

// MigrateBody is the optional JSON body of POST /v1/vms/{id}/migrate. Every
// field is optional from the server's view, but the CLI always sets Live (the
// --offline flag maps to live=false; the default is true). TargetNode /
// TargetPool name a node / pool on the target (omitted = scheduler picks, spec
// D2). MaxBandwidthBytes / MaxDowntimeMs are ephemeral knobs echoed onto the
// migration row. Pointer fields stay omitempty so an unset knob lets the server
// apply its default rather than pinning a zero.
type MigrateBody struct {
	TargetNode        *string `json:"target_node,omitempty"`
	TargetPool        *string `json:"target_pool,omitempty"`
	Live              bool    `json:"live"`
	AllowPostcopy     bool    `json:"allow_postcopy,omitempty"`
	MaxBandwidthBytes *int64  `json:"max_bandwidth_bytes,omitempty"`
	MaxDowntimeMs     *int32  `json:"max_downtime_ms,omitempty"`
}

// MigrateResult carries the outcome of StartMigration. The migrate endpoint is
// polymorphic: an explicit target equal to the VM's current node short-circuits
// to 200 already_on_target (a no-op, NOT an error), while every other accepted
// request returns 202 + an AsyncTaskAccepted. NoOp distinguishes the two:
// when true, Task is zero and CurrentNodeID names the node the VM already sits
// on; when false, Task carries the enqueued migration task.
type MigrateResult struct {
	NoOp          bool
	CurrentNodeID string
	Task          TaskAccepted
}

// Migration mirrors the migrationView the CP serves from GET /v1/migrations/{id}
// and GET /v1/migrations. The nullable fields (target_node_id, scheduling_reason,
// error_message, started_at, completed_at, byte counters, knobs) use pointers so
// the CLI can render "<unset>" / omit them cleanly. snake_case tags match the
// server wire shape exactly.
type Migration struct {
	ID               string  `json:"id"`
	VMID             string  `json:"vm_id"`
	VMName           string  `json:"vm_name"`
	SourceNodeID     *string `json:"source_node_id"`
	SourceNodeName   *string `json:"source_node_name"`
	TargetNodeID     *string `json:"target_node_id"`
	TargetNodeName   *string `json:"target_node_name"`
	Reason           string  `json:"reason"`
	Phase            string  `json:"phase"`
	ProgressPercent  int     `json:"progress_percent"`
	BytesTotal       *int64  `json:"bytes_total"`
	BytesTransferred int64   `json:"bytes_transferred"`
	BytesRemaining   *int64  `json:"bytes_remaining"`
	SchedulingReason *string `json:"scheduling_reason"`
	ErrorMessage     *string `json:"error_message"`
	Live             bool    `json:"live"`
	AllowPostcopy    bool    `json:"allow_postcopy"`
	TargetPoolName   string  `json:"target_pool_name"`
	Stalled          bool    `json:"stalled"`

	MaxBandwidthBytes *int64  `json:"max_bandwidth_bytes"`
	MaxDowntimeMs     *int32  `json:"max_downtime_ms"`
	StartedAt         *string `json:"started_at"`
	CompletedAt       *string `json:"completed_at"`
	CreatedAt         string  `json:"created_at"`
	UpdatedAt         string  `json:"updated_at"`

	Stats *MigrationStats `json:"stats"`
}

// MigrationStats mirrors the nested stats object the CP serves on a completed
// live migration. nil for offline / incomplete migrations.
type MigrationStats struct {
	RAM         MigrationRAMStats  `json:"ram"`
	Disk        MigrationDiskStats `json:"disk"`
	TotalTimeMs int64              `json:"total_time_ms"`
	DowntimeMs  int64              `json:"downtime_ms"`
	SetupTimeMs int64              `json:"setup_time_ms"`
}

type MigrationRAMStats struct {
	Transferred    int64 `json:"transferred"`
	Total          int64 `json:"total"`
	DirtyPagesRate int64 `json:"dirty_pages_rate"`
}

type MigrationDiskStats struct {
	Transferred int64 `json:"transferred"`
	Total       int64 `json:"total"`
}

// MigrationList is the {data, meta} envelope GET /v1/migrations serves.
type MigrationList struct {
	Data []Migration `json:"data"`
	Meta struct {
		NextCursor *string `json:"next_cursor"`
	} `json:"meta"`
}

// ListMigrationsParams bundles the query-string filters the list endpoint
// accepts. Empty fields are omitted from the URL so the server defaults
// (limit=50, no filter) take over. VM and Node are UUIDs (the server rejects
// non-UUIDs with 400) — the CLI forwards the raw operator string and lets the
// server validate.
type ListMigrationsParams struct {
	VM     string
	Node   string
	Limit  int
	Cursor string
}

// StartMigration submits POST /v1/vms/{vmID}/migrate. The endpoint is
// polymorphic: 200 already_on_target is a no-op (surfaced via
// MigrateResult.NoOp, NOT an error), 202 returns the enqueued task. Any non-2xx
// surfaces as *APIError (e.g. 409 conflict when a migration is already active).
func (c *Client) StartMigration(ctx context.Context, vmID string, body MigrateBody) (MigrateResult, error) {
	path := "/v1/vms/" + url.PathEscape(vmID) + "/migrate"
	httpReq, err := c.newRequest(ctx, http.MethodPost, path, body)
	if err != nil {
		return MigrateResult{}, err
	}
	status, raw, err := c.do(httpReq)
	if err != nil {
		return MigrateResult{}, err
	}
	if status == http.StatusOK {
		// 200 already_on_target — the explicit target equals the VM's current
		// node. Decode the no-op envelope distinctly from a 202 task.
		var noop struct {
			Status        string `json:"status"`
			CurrentNodeID string `json:"current_node_id"`
		}
		if err := decodeJSON(raw, &noop); err != nil {
			return MigrateResult{}, err
		}
		return MigrateResult{NoOp: true, CurrentNodeID: noop.CurrentNodeID}, nil
	}
	var task TaskAccepted
	if err := decodeJSON(raw, &task); err != nil {
		return MigrateResult{}, err
	}
	return MigrateResult{Task: task}, nil
}

// Migration fetches GET /v1/migrations/{id}. 200 returns the parsed view; 404 /
// 5xx surface as *APIError. The raw response body is returned alongside the
// decoded value so `migration get --output json` echoes the server projection
// verbatim; decode-only callers pass `_`.
func (c *Client) Migration(ctx context.Context, id string) (Migration, json.RawMessage, error) {
	httpReq, err := c.newRequest(ctx, http.MethodGet, "/v1/migrations/"+url.PathEscape(id), nil)
	if err != nil {
		return Migration{}, nil, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return Migration{}, nil, err
	}
	var out Migration
	if err := decodeJSON(body, &out); err != nil {
		return Migration{}, nil, err
	}
	return out, json.RawMessage(body), nil
}

// CancelMigration submits POST /v1/migrations/{id}/cancel. Cancel is sync and
// best-effort (spec D5): 200 returns the current Migration view, terminal or
// freshly cancelled (the endpoint is idempotent - cancelling an already-terminal
// migration returns it unchanged at 200). 404 / 5xx surface as *APIError.
func (c *Client) CancelMigration(ctx context.Context, id string) (Migration, error) {
	httpReq, err := c.newRequest(ctx, http.MethodPost, "/v1/migrations/"+url.PathEscape(id)+"/cancel", nil)
	if err != nil {
		return Migration{}, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return Migration{}, err
	}
	var out Migration
	if err := decodeJSON(body, &out); err != nil {
		return Migration{}, err
	}
	return out, nil
}

// ListMigrations fetches GET /v1/migrations with the supplied filters. Returns
// the decoded rows plus the opaque next-page cursor ("" when the page is the
// last). Non-2xx surfaces as *APIError.
func (c *Client) ListMigrations(ctx context.Context, params ListMigrationsParams) ([]Migration, string, error) {
	q := url.Values{}
	if params.VM != "" {
		q.Set("vm", params.VM)
	}
	if params.Node != "" {
		q.Set("node", params.Node)
	}
	if params.Limit > 0 {
		q.Set("limit", strconv.Itoa(params.Limit))
	}
	if params.Cursor != "" {
		q.Set("cursor", params.Cursor)
	}

	path := "/v1/migrations"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}

	httpReq, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, "", err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return nil, "", err
	}
	var out MigrationList
	if err := decodeJSON(body, &out); err != nil {
		return nil, "", err
	}
	next := ""
	if out.Meta.NextCursor != nil {
		next = *out.Meta.NextCursor
	}
	return out.Data, next, nil
}
