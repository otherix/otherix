// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/otherix/otherix/internal/api/response"
)

// CreateVMRequest mirrors the body the POST /v1/vms handler decodes.
// Field tags are the wire shape (vcpus / memory_mb), not the schema
// columns (cpu_cores / memory_mib) — the handler boundary owns the
// translation.
//
// The VM is created from one of two disk sources (the template entity is
// gone): exactly one of `ImageURL` or `SourceSnapshotID` is set. In the
// image-source mode `ImageURL` and `Arch` are required; the server
// downloads / caches the image into the target pool, `ImageSHA256` pins
// the expected digest, `Firmware` / `FirmwareID` select firmware by name
// or uuid, and `Format` / `DiskGiB` size the root disk. In the
// snapshot-source mode `SourceSnapshotID` is set and architecture /
// format / firmware come from the snapshot manifest. `pool` stays
// polymorphic per the multi-instance carve-out (either a pool name or a
// per-instance UUID literal). CLI callers pass operator-friendly names.
//
// Multi-instance pools + scheduler:
//   - `Pool` is optional. When empty, the server uses the cluster
//     default-pool (`PUT /v1/cluster/default-pool` to set); if
//     not set, returns 400 `default_pool_not_set`.
//   - `Node` is an optional placement hint. When non-nil, the
//     scheduler restricts placement to exactly that node. The VM is
//     admitted as pending; a pool-not-on-node mismatch keeps it pending
//     with `status.reason` `pool_not_on_node`. A uuid literal in `Node`
//     is rejected at create with 400 `validation_failed`.
type CreateVMRequest struct {
	Name        string `json:"name"`
	ImageURL    string `json:"image_url"`
	ImageSHA256 string `json:"image_sha256,omitempty"`
	Arch        string `json:"arch"`
	Firmware    string `json:"firmware,omitempty"`
	FirmwareID  string `json:"firmware_id,omitempty"`
	Format      string `json:"format,omitempty"`
	// ImagePullPolicy selects whether a cached image is reused
	// ("if_not_present", the server default) or re-fetched from ImageURL
	// ("always"). omitempty leaves the server default in force when unset.
	ImagePullPolicy string  `json:"image_pull_policy,omitempty"`
	DiskGiB         int     `json:"disk_gib,omitempty"`
	Pool            string  `json:"pool,omitempty"`
	Node            *string `json:"node,omitempty"`
	// SourceSnapshotID recreates the VM from a snapshot instead of an image.
	// Exactly one of ImageURL / SourceSnapshotID must be set. When set, the
	// server takes architecture / format / firmware from the snapshot manifest
	// and the resulting VM carries empty image fields. omitempty keeps it off
	// the wire for the image-sourced path.
	SourceSnapshotID string `json:"source_snapshot_id,omitempty"`
	VCPUs            int    `json:"vcpus"`
	MemoryMB         int    `json:"memory_mb"`
	// Network is the optional bridge network (name or uuid) to attach a
	// single NIC to. Omitted leaves the VM with no NIC (the agent falls
	// back to legacy SLIRP networking). The server rejects non-bridge
	// types with 400.
	Network string `json:"network,omitempty"`
	// UserData is the optional VM-level cloud-init user-data. When set,
	// it is passed through to the agent verbatim. Mutually exclusive
	// with CloudInitDisabled — server rejects both set with 400
	// validation_failed (CLI also gates this before dispatch).
	UserData *string `json:"user_data,omitempty"`
	// NetworkConfig is the optional VM-level cloud-init network-config
	// (NoCloud /network-config). Mutually exclusive with
	// CloudInitDisabled; allowed alongside UserData.
	NetworkConfig *string `json:"network_config,omitempty"`
	// CloudInitDisabled is the explicit-disable signal. When true,
	// the resolver returns empty user_data to the agent. omitempty lets
	// the JSON encoder skip the default-false case so existing handlers
	// that decode the historical wire shape still parse cleanly.
	CloudInitDisabled bool `json:"cloud_init_disabled,omitempty"`
	// SSHIngressEnabled opts the VM into SSH ingress: when true (and the
	// cluster master switch is on and a cluster SSH user-CA exists) the CP
	// provisions the guest to trust the cluster SSH user-CA. Mutually
	// exclusive with CloudInitDisabled. omitempty keeps the default-false
	// case off the wire.
	SSHIngressEnabled bool `json:"ssh_ingress_enabled,omitempty"`
	// Labels is the optional set of user-defined key/value tags stored on the
	// VM. Load balancers select their backend VMs by matching these labels, so
	// a VM must be labeled at create to be selectable. omitempty keeps an empty
	// map off the wire.
	Labels map[string]string `json:"labels,omitempty"`
}

// VM mirrors the response shape internal/api/handlers/vms.toView
// produces. The template entity is gone: a VM is created directly from
// an image source, so the view carries `image_url` / `image_sha256` /
// `format` instead of a template reference. `pool` carries
// storage_pools.name, `node` carries the agent-reported current
// location's name, `networks` carries the attached network names ordered
// by NIC device_order (empty for a VM with no NIC). owner_id keeps its
// UUID rendering; `owner` carries the owner's display_name (or email
// when unset) but only for callers holding user:read (admin /
// operator), nil otherwise. Architecture stays a string here
// (the CLI does not need the typed CpuArch enum for output formatting).
type VM struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	OwnerID     string  `json:"owner_id"`
	Owner       *string `json:"owner"`
	ImageURL    string  `json:"image_url"`
	ImageSHA256 string  `json:"image_sha256,omitempty"`
	Format      string  `json:"format"`
	// ImagePullPolicy echoes the VM's image_pull_policy ("if_not_present" |
	// "always") so `get -o yaml` round-trips it into spec.imagePullPolicy.
	ImagePullPolicy string   `json:"image_pull_policy"`
	Pool            string   `json:"pool"`
	Node            *string  `json:"node"`
	Networks        []string `json:"networks"`
	Architecture    string   `json:"architecture"`
	VCPUs           int      `json:"vcpus"`
	MemoryMB        int      `json:"memory_mb"`
	Status          VMStatus `json:"status"`
	DesiredPhase    string   `json:"desired_phase"`
	// SourceSnapshotID is the snapshot a VM was restored from, when it was
	// created in the snapshot-source mode (nil for image-sourced VMs). The
	// server surfaces it on GET /v1/vms/{id}; `vm get -o yaml` round-trips it
	// into spec.sourceSnapshotID so a snapshot-sourced VM manifest is not
	// reduced to an empty imageURL with no trace of its origin.
	SourceSnapshotID *string        `json:"source_snapshot_id"`
	Labels           map[string]any `json:"labels"`
	// UserData and NetworkConfig echo the cloud-init payloads supplied at
	// create time, verbatim (nil when unset). Set once at create and
	// immutable for the VM's lifetime. `vm get -o yaml` round-trips them
	// into an apply-ready manifest; the plain text view shows a presence
	// indicator only (the blobs can be large).
	UserData      *string `json:"user_data"`
	NetworkConfig *string `json:"network_config"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
}

// VMStatus is the nested system-reported runtime/scheduling status the
// server projects onto VM.status. Phase is the computed lifecycle phase
// (pending / creating / running / paused / stopped / error / ...).
// Reason and Message carry the machine-readable code and human text when
// the VM is still pending placement (e.g. pool_not_ready); both are empty
// once the VM has a running phase.
type VMStatus struct {
	Phase   string `json:"phase"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	// MemoryUsedMib is the guest-reported in-use memory (MiB) from
	// virtio-balloon stats. Best-effort observed state: nil when the VM is
	// not running or the guest balloon driver has not reported yet.
	MemoryUsedMib *int64 `json:"memory_used_mib,omitempty"`
}

// IsTerminalPhase reports whether the VM's scheduling/lifecycle phase has
// settled enough for `vm create --wait` to stop polling: a running VM is
// the success terminal, an error/failed phase is the failure terminal.
// Pending and creating are non-terminal (the scheduler / agent are still
// converging).
func (s VMStatus) IsTerminalPhase() bool {
	switch s.Phase {
	case "running", "error", "failed":
		return true
	}
	return false
}

// VMList is the {data, meta} envelope every list endpoint serves.
// next_cursor is nullable.
type VMList struct {
	Data []VM `json:"data"`
	Meta struct {
		NextCursor *string `json:"next_cursor"`
	} `json:"meta"`
}

// TaskAccepted mirrors response.AsyncTaskAccepted. Re-declared here
// so cpclient does not import the server-side response package.
type TaskAccepted struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
	Links  struct {
		Self string `json:"self"`
	} `json:"links"`
}

// ListVMsParams bundles the query-string filters the list endpoint
// accepts. Empty fields are omitted from the URL — the handler's
// defaults (limit=50, no filter) take over. Node is name-only; Pool
// stays polymorphic per the multi-instance carve-out (name or UUID).
type ListVMsParams struct {
	Limit  int
	Cursor string
	Pool   string
	Node   string
	Status string
}

// CreateVM submits a POST /v1/vms. Admission is split from scheduling:
// the server persists the VM in `pending` immediately and returns 201
// with the created VM view (status.phase == "pending"); a CP-side
// reconcile loop binds it to a (node, pool) later. The raw response body
// is returned alongside the decoded value so `vm create --output json`
// can echo the server's projection verbatim. Any non-2xx surfaces as
// *APIError.
func (c *Client) CreateVM(ctx context.Context, req CreateVMRequest) (VM, json.RawMessage, error) {
	if req.Name == "" {
		return VM{}, nil, errors.New("cpclient.CreateVM: name is required")
	}

	httpReq, err := c.newRequest(ctx, http.MethodPost, "/v1/vms", req)
	if err != nil {
		return VM{}, nil, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return VM{}, nil, err
	}

	var out VM
	if err := decodeJSON(body, &out); err != nil {
		return VM{}, nil, err
	}
	return out, json.RawMessage(body), nil
}

// VM fetches /v1/vms/{identifier}. The parameter is a VM name;
// UUID literals are rejected by the server with 400 validation_failed.
// 200 returns the parsed VM; 404 / 5xx surface as *APIError. The raw
// response body is returned alongside the decoded value so `vm get
// --output json` echoes the server's projection verbatim
// (absent-vs-null preserved); decode-only callers pass `_`.
func (c *Client) VM(ctx context.Context, identifier string) (VM, json.RawMessage, error) {
	httpReq, err := c.newRequest(ctx, http.MethodGet, "/v1/vms/"+url.PathEscape(identifier), nil)
	if err != nil {
		return VM{}, nil, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return VM{}, nil, err
	}
	var out VM
	if err := decodeJSON(body, &out); err != nil {
		return VM{}, nil, err
	}
	return out, json.RawMessage(body), nil
}

// ListVMs fetches /v1/vms with the supplied query filters. Returns the
// parsed envelope; pagination is the caller's responsibility (use
// VMList.Meta.NextCursor as the next page's params.Cursor).
func (c *Client) ListVMs(ctx context.Context, params ListVMsParams) (VMList, error) {
	q := url.Values{}
	if params.Limit > 0 {
		q.Set("limit", strconv.Itoa(params.Limit))
	}
	if params.Cursor != "" {
		q.Set("cursor", params.Cursor)
	}
	if params.Pool != "" {
		q.Set("pool", params.Pool)
	}
	if params.Node != "" {
		q.Set("node", params.Node)
	}
	if params.Status != "" {
		q.Set("status", params.Status)
	}

	path := "/v1/vms"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}

	httpReq, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return VMList{}, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return VMList{}, err
	}
	var out VMList
	if err := decodeJSON(body, &out); err != nil {
		return VMList{}, err
	}
	return out, nil
}

// PauseVM submits POST /v1/vms/{identifier}/pause — synchronous per
// the CP spec. 200 returns the refreshed VM with phase=paused; 404 /
// 409 / 5xx surface as *APIError.
func (c *Client) PauseVM(ctx context.Context, identifier string) (VM, error) {
	return c.postVMLifecycle(ctx, identifier, "pause")
}

// ResumeVM submits POST /v1/vms/{identifier}/resume — synchronous
// per the CP spec. 200 returns the refreshed VM with phase=running.
func (c *Client) ResumeVM(ctx context.Context, identifier string) (VM, error) {
	return c.postVMLifecycle(ctx, identifier, "resume")
}

// ResetVM submits POST /v1/vms/{identifier}/reset — synchronous per
// the spec amendment. 200 returns the refreshed VM; phase
// stays `running` because reset preserves runtime identity (the
// QEMU process keeps running, only the guest CPU is reset).
func (c *Client) ResetVM(ctx context.Context, identifier string) (VM, error) {
	return c.postVMLifecycle(ctx, identifier, "reset")
}

// postVMLifecycle is the shared engine for PauseVM / ResumeVM /
// ResetVM. The CP endpoint shape is identical — POST, empty body,
// 200 returns the refreshed VM view.
func (c *Client) postVMLifecycle(ctx context.Context, identifier, action string) (VM, error) {
	path := "/v1/vms/" + url.PathEscape(identifier) + "/" + action
	httpReq, err := c.newRequest(ctx, http.MethodPost, path, nil)
	if err != nil {
		return VM{}, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return VM{}, err
	}
	var out VM
	if err := decodeJSON(body, &out); err != nil {
		return VM{}, err
	}
	return out, nil
}

// StartVM submits POST /v1/vms/{identifier}/start — async per spec.
// 202 returns TaskAccepted; clients poll the task for terminal state.
func (c *Client) StartVM(ctx context.Context, identifier string) (TaskAccepted, error) {
	return c.postVMLifecycleAsync(ctx, identifier, "start")
}

// StopVM submits POST /v1/vms/{identifier}/stop — async graceful
// ACPI shutdown. If the guest does not honour the signal within the
// server-side timeout the task fails with `stop_timeout` (operators
// dispatch to poweroff or `stop --force` to force).
func (c *Client) StopVM(ctx context.Context, identifier string) (TaskAccepted, error) {
	return c.postVMLifecycleAsync(ctx, identifier, "stop")
}

// PoweroffVM submits POST /v1/vms/{identifier}/poweroff — async
// hard shutdown. The guest OS is not notified.
func (c *Client) PoweroffVM(ctx context.Context, identifier string) (TaskAccepted, error) {
	return c.postVMLifecycleAsync(ctx, identifier, "poweroff")
}

// RebootVM submits POST /v1/vms/{identifier}/reboot — async stop+
// start cycle (distinct from Reset; the QEMU PID changes).
func (c *Client) RebootVM(ctx context.Context, identifier string) (TaskAccepted, error) {
	return c.postVMLifecycleAsync(ctx, identifier, "reboot")
}

// postVMLifecycleAsync is the shared engine for the four async
// lifecycle CLI subcommands. The CP endpoint shape is identical —
// POST, empty body, 202 returns TaskAccepted.
func (c *Client) postVMLifecycleAsync(ctx context.Context, identifier, action string) (TaskAccepted, error) {
	path := "/v1/vms/" + url.PathEscape(identifier) + "/" + action
	httpReq, err := c.newRequest(ctx, http.MethodPost, path, nil)
	if err != nil {
		return TaskAccepted{}, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return TaskAccepted{}, err
	}
	var out TaskAccepted
	if err := decodeJSON(body, &out); err != nil {
		return TaskAccepted{}, err
	}
	return out, nil
}

// DeleteVM submits a DELETE /v1/vms/{identifier}. The identifier is
// a VM name; UUID literals are rejected by the server with 400
// validation_failed. 202 returns the parsed TaskAccepted; 404
// surfaces as *APIError so callers can decide whether to treat absent
// VMs as success (typical for retry loops).
func (c *Client) DeleteVM(ctx context.Context, identifier string) (TaskAccepted, error) {
	httpReq, err := c.newRequest(ctx, http.MethodDelete, "/v1/vms/"+url.PathEscape(identifier), nil)
	if err != nil {
		return TaskAccepted{}, err
	}
	status, body, err := c.do(httpReq)
	if err != nil {
		return TaskAccepted{}, err
	}
	// A pending (unscheduled) VM is deleted CP-side and returns 204 No Content
	// with no body - there is no agent-teardown task to track. A scheduled VM
	// returns 202 + an AsyncTaskAccepted. An empty TaskAccepted (TaskID == "")
	// signals the synchronous-delete case to the caller.
	if status == http.StatusNoContent || len(body) == 0 {
		return TaskAccepted{}, nil
	}
	var out TaskAccepted
	if err := decodeJSON(body, &out); err != nil {
		return TaskAccepted{}, err
	}
	return out, nil
}

// VMConsoleRequest is the body POSTed to /v1/vms/{id}/console. Empty
// protocol defaults to "vnc" CP-side; CLI callers pass "serial" to
// drive the operator-facing console subcommand.
type VMConsoleRequest struct {
	Protocol string `json:"protocol,omitempty"`
}

// VMConsoleResponse mirrors the OpenAPI VMConsoleResponse schema. The
// token is single-use, 30s TTL on the agent side; websocket_url
// already embeds the token as the `?token=...` query parameter — the
// CLI just opens the URL and lets the WebSocket dial handle the rest.
type VMConsoleResponse struct {
	Token        string `json:"token"`
	WebsocketURL string `json:"websocket_url"`
	Protocol     string `json:"protocol"`
	ExpiresAt    string `json:"expires_at"`
}

// VMConsole issues a console-session ticket against `POST /v1/vms/{id}/console`.
// 200 returns the parsed envelope; 401 / 403 / 404 / 409 / 5xx surface
// as *APIError.
func (c *Client) VMConsole(ctx context.Context, vmName, protocol string) (VMConsoleResponse, error) {
	if vmName == "" {
		return VMConsoleResponse{}, errors.New("cpclient.VMConsole: vm name is required")
	}
	req := VMConsoleRequest{Protocol: protocol}
	httpReq, err := c.newRequest(ctx, http.MethodPost,
		"/v1/vms/"+url.PathEscape(vmName)+"/console", req)
	if err != nil {
		return VMConsoleResponse{}, err
	}
	_, body, err := c.do(httpReq)
	if err != nil {
		return VMConsoleResponse{}, err
	}
	var out VMConsoleResponse
	if err := decodeJSON(body, &out); err != nil {
		return VMConsoleResponse{}, err
	}
	return out, nil
}

// VMLogs opens GET /v1/vms/{name}/logs and returns the streaming
// response body. The caller is responsible for Close. tailLines
// follows kubectl semantics: -1 (default) yields the full retained
// history, 0 skips history entirely, positive N returns the trailing
// N lines. follow keeps the stream open for live bytes after the
// initial history flushes.
//
// Non-2xx envelopes surface as *APIError before any bytes are
// returned to the caller; on success the returned reader streams the
// agent's chunked text/plain body verbatim.
func (c *Client) VMLogs(ctx context.Context, vmName string, tailLines int, follow bool) (io.ReadCloser, error) {
	if vmName == "" {
		return nil, errors.New("cpclient.VMLogs: vm name is required")
	}
	qry := url.Values{}
	qry.Set("tail", strconv.Itoa(tailLines))
	if follow {
		qry.Set("follow", "true")
	}
	path := "/v1/vms/" + url.PathEscape(vmName) + "/logs?" + qry.Encode()

	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "text/plain")

	// The shared httpClient carries a per-request Timeout to bound
	// regular REST calls; streaming logs would hit that timeout. Wrap
	// the same Transport in a fresh client without a Timeout so TLS
	// trust and connection pooling are preserved.
	transport := c.httpClient.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	streamClient := &http.Client{Transport: transport, Jar: c.httpClient.Jar}

	resp, err := streamClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cpclient: VMLogs: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, parseAPIError(resp.StatusCode, body)
	}
	return resp.Body, nil
}

// Compile-time check that response.AsyncTaskAccepted shares the same
// JSON shape as our local TaskAccepted (catches drift if the server
// envelope changes). Built only for local sanity; the value isn't
// used at runtime.
var _ = response.AsyncTaskAccepted{}
