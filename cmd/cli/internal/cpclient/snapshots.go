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

// SnapshotCreateBody is the JSON body of POST /v1/vms/{id}/snapshots. Name is
// required; Description is optional. WithMemory stays in the wire shape for
// forward compatibility but the server rejects true with 400 in slice A
// (RAM capture unsupported). omitempty keeps Description / WithMemory off the
// wire when unset so the server defaults apply.
type SnapshotCreateBody struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	WithMemory  bool   `json:"with_memory,omitempty"`
}

// SnapshotDisk is one content-addressed disk blob descriptor in a snapshot
// manifest, ordered by disk index (virtio0, virtio1, ...).
type SnapshotDisk struct {
	Index     int    `json:"index"`
	Device    string `json:"device"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

// Snapshot mirrors the VmSnapshot view the CP serves from
// GET /v1/snapshots/{id} and GET /v1/vms/{id}/snapshots. Nullable fields use
// pointers so the CLI can render "-" / omit them; snake_case tags match the
// server wire shape exactly.
type Snapshot struct {
	ID                string         `json:"id"`
	VMID              string         `json:"vm_id"`
	ParentSnapshotID  *string        `json:"parent_snapshot_id"`
	OwnerID           string         `json:"owner_id"`
	Name              string         `json:"name"`
	Description       string         `json:"description"`
	Status            string         `json:"status"`
	WithMemory        bool           `json:"with_memory"`
	VMStateAtSnapshot string         `json:"vm_state_at_snapshot"`
	Architecture      string         `json:"architecture"`
	FirmwareID        *string        `json:"firmware_id"`
	Disks             []SnapshotDisk `json:"disks"`
	DiskSizeBytes     *int64         `json:"disk_size_bytes"`
	ErrorMessage      *string        `json:"error_message"`
	CreatedAt         string         `json:"created_at"`
	UpdatedAt         string         `json:"updated_at"`
}

// SnapshotListItem is the global-list projection GET /v1/snapshots serves: the
// base Snapshot view plus the operator-facing vm_name + owner_display_name the
// CP resolves server-side. Both pointers are nil only when the referenced row is
// gone (force-deleted VM / user); the id fields stay authoritative. Mirrors the
// server's snapshotListItemView.
type SnapshotListItem struct {
	Snapshot
	VMName           *string `json:"vm_name"`
	OwnerDisplayName *string `json:"owner_display_name"`
}

// SnapshotList is the {data, meta} envelope GET /v1/snapshots serves.
type SnapshotList struct {
	Data []SnapshotListItem `json:"data"`
	Meta struct {
		NextCursor *string `json:"next_cursor"`
	} `json:"meta"`
}

// ListSnapshotsParams bundles the cursor-pagination query for the global list
// endpoint GET /v1/snapshots. Empty fields are omitted from the URL so the
// server defaults (limit=50, first page, no filter) take over. VM and Owner are
// UUIDs (the server rejects non-UUIDs with 400) — the CLI forwards the raw
// operator string and lets the server validate. Status filters on the snapshot
// status enum.
type ListSnapshotsParams struct {
	VM     string
	Status string
	Owner  string
	Limit  int
	Cursor string
}

// CreateSnapshot submits POST /v1/vms/{vmIdentifier}/snapshots. The VM
// identifier is a VM name; UUID literals are rejected by the server. 202
// returns the enqueued task; any non-2xx surfaces as *APIError (e.g. 409
// conflict on a duplicate snapshot name, 400 validation_failed on
// with_memory=true). Mirrors StartMigration.
func (c *Client) CreateSnapshot(ctx context.Context, vmIdentifier string, body SnapshotCreateBody) (TaskAccepted, error) {
	path := "/v1/vms/" + url.PathEscape(vmIdentifier) + "/snapshots"
	httpReq, err := c.newRequest(ctx, http.MethodPost, path, body)
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

// Snapshot fetches GET /v1/snapshots/{id}. 200 returns the parsed view; 404 /
// 5xx surface as *APIError. The raw response body is returned alongside the
// decoded value so `vm snapshot get --output json` echoes the server
// projection verbatim; decode-only callers pass `_`. Mirrors Migration.
func (c *Client) Snapshot(ctx context.Context, id string) (Snapshot, json.RawMessage, error) {
	httpReq, err := c.newRequest(ctx, http.MethodGet, "/v1/snapshots/"+url.PathEscape(id), nil)
	if err != nil {
		return Snapshot{}, nil, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return Snapshot{}, nil, err
	}
	var out Snapshot
	if err := decodeJSON(body, &out); err != nil {
		return Snapshot{}, nil, err
	}
	return out, json.RawMessage(body), nil
}

// ListSnapshotsGlobal fetches GET /v1/snapshots, the global snapshot
// collection, with the supplied cursor-pagination params and optional
// vm / status / owner filters. Returns the decoded rows (each carrying the
// resolved vm_name + owner_display_name) plus the opaque next-page cursor ("" on
// the last page). Non-2xx surfaces as *APIError. Mirrors ListMigrations.
func (c *Client) ListSnapshotsGlobal(ctx context.Context, params ListSnapshotsParams) ([]SnapshotListItem, string, error) {
	q := url.Values{}
	if params.VM != "" {
		q.Set("vm", params.VM)
	}
	if params.Status != "" {
		q.Set("status", params.Status)
	}
	if params.Owner != "" {
		q.Set("owner", params.Owner)
	}
	if params.Limit > 0 {
		q.Set("limit", strconv.Itoa(params.Limit))
	}
	if params.Cursor != "" {
		q.Set("cursor", params.Cursor)
	}

	path := "/v1/snapshots"
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
	var out SnapshotList
	if err := decodeJSON(body, &out); err != nil {
		return nil, "", err
	}
	next := ""
	if out.Meta.NextCursor != nil {
		next = *out.Meta.NextCursor
	}
	return out.Data, next, nil
}

// DeleteSnapshot submits DELETE /v1/snapshots/{id}. 202 returns the enqueued
// teardown task; 404 / 409 (fail-closed: snapshot has children) / 5xx surface
// as *APIError.
func (c *Client) DeleteSnapshot(ctx context.Context, id string) (TaskAccepted, error) {
	httpReq, err := c.newRequest(ctx, http.MethodDelete, "/v1/snapshots/"+url.PathEscape(id), nil)
	if err != nil {
		return TaskAccepted{}, err
	}
	status, body, err := c.do(httpReq)
	if err != nil {
		return TaskAccepted{}, err
	}
	// A snapshot that never reached the agent (still creating CP-side) may be
	// deleted synchronously with 204 No Content and no body - there is no
	// agent-teardown task to track. An empty TaskAccepted (TaskID == "")
	// signals that case to the caller.
	if status == http.StatusNoContent || len(body) == 0 {
		return TaskAccepted{}, nil
	}
	var out TaskAccepted
	if err := decodeJSON(body, &out); err != nil {
		return TaskAccepted{}, err
	}
	return out, nil
}
