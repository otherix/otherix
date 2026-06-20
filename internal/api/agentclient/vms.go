// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
)

// VMCreateRequest is the body the CP-side worker sends to the agent's
// POST /v1/vms. Wire-shape matches the agent's actual
// surface (internal/agent/handlers/vms/create.go) — minimal fields
// only. The richer agent.yaml VMSpec is a future design target;
// reconciliation between the two is tracked as future work.
//
// The `pool` field carries the pool name, not a UUID. The agent's
// local pool registry is name-keyed; the cluster-wide multi-instance
// UUID polymorphism stays an operator-edge concern. CP-side worker
// reads `pool.Name` from the loaded `store.StoragePool` row before
// constructing this request.
//
// UUID carries the CP-minted vms.id — agent uses it as the per-VM
// identity instead of generating its own. Must be non-nil when called
// from the CP worker.
type VMCreateRequest struct {
	UUID     uuid.UUID `json:"uuid"`
	Name     string    `json:"name"`
	VCPUs    int       `json:"vcpus"`
	MemoryMB int       `json:"memory_mb"`
	Pool     string    `json:"pool"`
	// ImageURL is the HTTPS source the agent ensures into the pool's
	// basename-keyed cache (IfNotPresent). ExpectedSHA256, when non-empty,
	// pins the content digest the agent verifies after fetch. Format
	// defaults to "qcow2" agent-side when empty. DiskGiB is the requested
	// root-disk size in GiB (0 = default to the image's virtual size).
	ImageURL       string `json:"image_url"`
	ExpectedSHA256 string `json:"expected_sha256,omitempty"`
	Format         string `json:"format,omitempty"`
	DiskGiB        int    `json:"disk_gib,omitempty"`
	// ResolvedImageDigest is the CP's best-effort content-digest HINT for an
	// UNPINNED image. When set the CP has pre-pulled this digest onto the target
	// from a peer, so the agent clones from its image cache instead of
	// downloading ImageURL. Not a pin: a cache miss falls back to download. Nil
	// for pinned creates and for pull_policy=always.
	ResolvedImageDigest *string `json:"resolved_image_digest,omitempty"`
	// UserData carries the CP-resolved cloud-init `#cloud-config`
	// blob (L3 Area 3 lock — already-resolved vm.user_data,
	// hostname-injected (no template fallback)). Empty
	// string on the wire means "no cidata"; the agent skips
	// ISO generation when absent.
	UserData string `json:"user_data,omitempty"`
	// NetworkConfig carries the operator-supplied cloud-init
	// network-config blob (NoCloud /network-config), passed through
	// verbatim. Empty means the agent writes no /network-config.
	NetworkConfig string `json:"network_config,omitempty"`
	// CloudInitDisabled is the explicit opt-out forwarded from the VM row. The
	// agent attaches a minimal always-on NoCloud seed unless this is true, so
	// empty UserData no longer implies "no seed" - the flag must travel.
	CloudInitDisabled bool `json:"cloud_init_disabled,omitempty"`
	// SourceSnapshot, when non-nil, tells the agent to recreate the VM's disks
	// from a snapshot's content-addressed blobs instead of downloading ImageURL.
	// The CP-side worker resolves the snapshot manifest (ordered disk digests +
	// the pool the blobs live on) and sets this; ImageURL is empty in that case.
	// This is the SEAM: the agent has no Control-Plane store access, so it can
	// only materialize from the resolved digests the worker hands it.
	SourceSnapshot *VMCreateSourceSnapshot `json:"source_snapshot,omitempty"`
	// Nics are the fully-resolved network interfaces to attach. The
	// CP-side worker resolves each vm_nic row against its network
	// (bridge_name + mtu come from the network; mac/model/order from
	// the vm_nic). Absent or empty means legacy SLIRP user-mode
	// networking on the agent.
	Nics []VMCreateNIC `json:"nics,omitempty"`
}

// VMCreateSourceSnapshot is the resolved snapshot disk source in a
// VMCreateRequest: the pool whose snapshots/ subdir holds the blobs plus the
// ordered per-disk blob digests the agent clones (virtio<i> order).
type VMCreateSourceSnapshot struct {
	Pool  string                       `json:"pool"`
	Disks []VMCreateSourceSnapshotDisk `json:"disks"`
}

// VMCreateSourceSnapshotDisk is one disk in a VMCreateSourceSnapshot: virtio
// index, the wire device name (virtio<i>), and the content-addressed blob
// sha256 the agent clones from the pool's snapshots/ cache.
type VMCreateSourceSnapshotDisk struct {
	Index  int    `json:"index"`
	Device string `json:"device"`
	SHA256 string `json:"sha256"`
}

// VMCreateNIC is one resolved network interface in a VMCreateRequest. The
// wire shape mirrors the agent's nicReq (internal/agent/handlers/vms): the
// agent reads `id` as the per-NIC identity, `bridge`/`mtu` to materialise the
// tap, and `mac`/`model`/`device_order` to build the QEMU device.
type VMCreateNIC struct {
	ID          uuid.UUID `json:"id"`
	Bridge      string    `json:"bridge"`
	MAC         string    `json:"mac"`
	Model       string    `json:"model"`
	MTU         int       `json:"mtu"`
	DeviceOrder int       `json:"device_order"`
}

// VMCreateResult is the agent's terminal vm.create task result: the resolved
// image content digest, the image's virtual size, and the materialized disk
// size (in bytes). The CP records the disk size onto vm_disk_runtime and the
// sha onto the VM row.
type VMCreateResult struct {
	VMID             string `json:"vm_id"`
	ImageSHA256      string `json:"image_sha256"`
	VirtualSizeBytes int64  `json:"virtual_size_bytes"`
	DiskSizeBytes    int64  `json:"disk_size_bytes"`
}

// AgentVM mirrors the agent's vmView wire shape (handler.go). Internal
// agent fields (qemu_pid, qmp_socket_path, raw VNC/SPICE ports,
// on-node file paths) are intentionally NOT exposed here per CLAUDE.md
// "VM-domain resources" rule — runtime detail is observed-state
// surfaced through CP's vm_runtime row, not propagated through the
// agentclient contract.
//
// The `pool` field carries the pool name. The agent's local registry
// is name-keyed; the per-instance row UUID stays a CP-edge concern.
type AgentVM struct {
	ID           uuid.UUID `json:"id"`
	Name         string    `json:"name"`
	VCPUs        int       `json:"vcpus"`
	MemoryMB     int       `json:"memory_mb"`
	Pool         string    `json:"pool"`
	Architecture string    `json:"architecture"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	// BootDiskVirtualSizeBytes is the agent-reported actual virtual size of the
	// on-disk boot qcow2 (qemu-img info -U), independent of the requested
	// disk_gib. The CP uses it to size a live-migration destination disk to the
	// real source size. 0 means the agent could not probe it (treat as unknown).
	BootDiskVirtualSizeBytes int64 `json:"boot_disk_virtual_size_bytes"`
}

// agentVMListResponse mirrors the agent's listResponse wire shape
// (handler.go list.go). The meta envelope is forward-compatible with
// cursor pagination but the agent currently leaves next_cursor null.
type agentVMListResponse struct {
	Data []AgentVM      `json:"data"`
	Meta map[string]any `json:"meta"`
}

// PostVMCreate submits a vm.create request to the agent and returns
// the agent's task id from the AsyncTaskAccepted envelope. 202 is the
// happy path; any non-2xx response surfaces as *AgentError.
//
// idempotencyKey is forwarded as the agent-side Idempotency-Key
// header. The agent's idempotency scope is independent of the CP's
// user-facing scope; the CP-side worker passes the stable CP task id
// (so a redelivered attempt reuses the same key), and dedup is keyed by
// agent_task_id resumption plus the agent's durable per-vm-id check.
func (c *Client) PostVMCreate(
	ctx context.Context,
	endpoint string,
	idempotencyKey string,
	req VMCreateRequest,
) (uuid.UUID, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return uuid.Nil, fmt.Errorf("agentclient: encode VMCreateRequest: %v", err)
	}

	target, _ := url.JoinPath(endpoint, "/v1/vms")
	httpReq, err := newRequest(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return uuid.Nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		httpReq.Header.Set("Idempotency-Key", idempotencyKey)
	}

	_, respBody, err := c.do(httpReq)
	if err != nil {
		return uuid.Nil, fmt.Errorf("agentclient: post vm create: %w", err)
	}

	var accepted agentapi.AsyncTaskAccepted
	if err := json.Unmarshal(respBody, &accepted); err != nil {
		return uuid.Nil, fmt.Errorf("agentclient: decode AsyncTaskAccepted: %v", err)
	}
	if accepted.TaskID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("agentclient: AsyncTaskAccepted.task_id is zero uuid")
	}
	return accepted.TaskID, nil
}

// VM fetches the agent's view of a single VM. 200 returns a parsed
// AgentVM; 404 / 5xx surface as *AgentError; malformed body as a
// wrapped generic error. The agent's `GET /v1/vms/{vm_name}` is
// name-keyed; callers pass the same name the CP minted at VM-create.
func (c *Client) VM(ctx context.Context, endpoint string, vmName string) (AgentVM, error) {
	var zero AgentVM

	target, _ := url.JoinPath(endpoint, "/v1/vms/"+vmName)
	req, err := newRequest(ctx, http.MethodGet, target, nil)
	if err != nil {
		return zero, err
	}

	_, body, err := c.do(req)
	if err != nil {
		return zero, fmt.Errorf("agentclient: get vm: %w", err)
	}

	var v AgentVM
	if err := json.Unmarshal(body, &v); err != nil {
		return zero, fmt.Errorf("agentclient: decode AgentVM: %v", err)
	}
	return v, nil
}

// ListVMs returns the agent's full VM inventory. The agent currently
// returns a single page without pagination; cursor pagination may be
// layered on later, but this method's signature stays the same since
// the meta envelope holds the cursor server-side.
func (c *Client) ListVMs(ctx context.Context, endpoint string) ([]AgentVM, error) {
	target, _ := url.JoinPath(endpoint, "/v1/vms")
	req, err := newRequest(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}

	_, body, err := c.do(req)
	if err != nil {
		return nil, fmt.Errorf("agentclient: list vms: %w", err)
	}

	var resp agentVMListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("agentclient: decode vm list: %v", err)
	}
	return resp.Data, nil
}

// PauseVM issues a synchronous POST /v1/vms/{vm_name}/pause against
// the agent and returns the refreshed AgentVM. 200 is the happy
// path; 404 / 409 / 5xx surface as *AgentError so the CP handler
// can translate downstream codes (409 conflict for invalid state)
// verbatim to the operator envelope.
func (c *Client) PauseVM(ctx context.Context, endpoint, vmName, idempotencyKey string) (AgentVM, error) {
	return c.postVMLifecycle(ctx, endpoint, "pause", vmName, idempotencyKey)
}

// ResumeVM issues a synchronous POST /v1/vms/{vm_name}/resume. Same
// envelope contract as PauseVM.
func (c *Client) ResumeVM(ctx context.Context, endpoint, vmName, idempotencyKey string) (AgentVM, error) {
	return c.postVMLifecycle(ctx, endpoint, "resume", vmName, idempotencyKey)
}

// ResetVM issues a synchronous POST /v1/vms/{vm_name}/reset. Same
// envelope contract as PauseVM. Reset is
// sync (200 + VMDetail) rather than 202 + task.
func (c *Client) ResetVM(ctx context.Context, endpoint, vmName, idempotencyKey string) (AgentVM, error) {
	return c.postVMLifecycle(ctx, endpoint, "reset", vmName, idempotencyKey)
}

// postVMLifecycle is the shared engine for the three sync lifecycle
// calls. The agent endpoint shape is identical — POST, no body, 200
// returns the refreshed VM view — so the dispatch sites differ only
// in the URL action segment.
func (c *Client) postVMLifecycle(
	ctx context.Context,
	endpoint, action, vmName, idempotencyKey string,
) (AgentVM, error) {
	var zero AgentVM
	target, _ := url.JoinPath(endpoint, "/v1/vms/"+vmName+"/"+action)
	req, err := newRequest(ctx, http.MethodPost, target, nil)
	if err != nil {
		return zero, err
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	_, body, err := c.do(req)
	if err != nil {
		return zero, fmt.Errorf("agentclient: %s vm: %w", action, err)
	}
	var v AgentVM
	if err := json.Unmarshal(body, &v); err != nil {
		return zero, fmt.Errorf("agentclient: decode AgentVM after %s: %v", action, err)
	}
	return v, nil
}

// StartVM submits an async POST /v1/vms/{vm_name}/start to the agent
// and returns the agent's task id. Mirrors DeleteVM's wire pattern: 202
// is the happy path; non-2xx surface as *AgentError so the CP worker
// can classify (404 → not_found, 409 → conflict, etc.).
func (c *Client) StartVM(ctx context.Context, endpoint, vmName, idempotencyKey string) (uuid.UUID, error) {
	return c.postVMLifecycleAsync(ctx, endpoint, "start", vmName, idempotencyKey)
}

// StopVM submits POST /v1/vms/{vm_name}/stop. Same envelope contract
// as StartVM. Stop is a graceful ACPI shutdown — the agent's task may
// terminate with `stop_timeout` if the guest does not honour the
// signal (Area 4-II lock — no internal escalation; operators dispatch
// to poweroff or `stop --force`).
func (c *Client) StopVM(ctx context.Context, endpoint, vmName, idempotencyKey string) (uuid.UUID, error) {
	return c.postVMLifecycleAsync(ctx, endpoint, "stop", vmName, idempotencyKey)
}

// PoweroffVM submits POST /v1/vms/{vm_name}/poweroff. Hard shutdown
// path; the agent attempts QMP `quit` first and falls back to SIGKILL
// after poweroffGrace (5s). The guest OS is not notified.
func (c *Client) PoweroffVM(ctx context.Context, endpoint, vmName, idempotencyKey string) (uuid.UUID, error) {
	return c.postVMLifecycleAsync(ctx, endpoint, "poweroff", vmName, idempotencyKey)
}

// RebootVM submits POST /v1/vms/{vm_name}/reboot. The agent
// orchestrates an internal stop+start (Area 4-III lock — distinct
// from Reset; the QEMU process is replaced so the PID changes). On
// stop-phase timeout the agent task fails with `stop_timeout`.
func (c *Client) RebootVM(ctx context.Context, endpoint, vmName, idempotencyKey string) (uuid.UUID, error) {
	return c.postVMLifecycleAsync(ctx, endpoint, "reboot", vmName, idempotencyKey)
}

// IssueConsoleTokenRequest is the body posted to the agent's
// `POST /v1/vms/{vm_name}/console-token`. Protocol selects the wire
// format the stream endpoint will speak after WebSocket upgrade.
type IssueConsoleTokenRequest struct {
	Protocol string `json:"protocol"`
}

// IssueConsoleTokenResponse mirrors the agent's ConsoleTokenResponse
// schema (api/openapi/agent.yaml). The CP combines `websocket_path`
// with the agent's external host (direct mode) or with the CP's own host
// (proxy mode) to build the user-facing `wss://` URL.
type IssueConsoleTokenResponse struct {
	Token         string    `json:"token"`
	ExpiresAt     time.Time `json:"expires_at"`
	WebsocketPath string    `json:"websocket_path"`
	Protocol      string    `json:"protocol"`
}

// IssueConsoleToken posts to the agent's console-token endpoint and
// returns the agent's response. The token is single-use, TTL 30
// seconds; the caller (CP handler `vms.console`) hands the
// plaintext value back to the operator immediately. Plaintext is never
// persisted on the CP — agents are the only authority for token
// state.
//
// The path is name-keyed (`/v1/vms/{vm_name}/...`).
func (c *Client) IssueConsoleToken(
	ctx context.Context,
	endpoint, vmName, protocol string,
) (IssueConsoleTokenResponse, error) {
	var zero IssueConsoleTokenResponse
	body, err := json.Marshal(IssueConsoleTokenRequest{Protocol: protocol})
	if err != nil {
		return zero, fmt.Errorf("agentclient: encode IssueConsoleTokenRequest: %v", err)
	}
	target, _ := url.JoinPath(endpoint, "/v1/vms/"+vmName+"/console-token")
	req, err := newRequest(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return zero, err
	}
	req.Header.Set("Content-Type", "application/json")

	_, respBody, err := c.do(req)
	if err != nil {
		return zero, fmt.Errorf("agentclient: issue console token: %w", err)
	}
	var parsed IssueConsoleTokenResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return zero, fmt.Errorf("agentclient: decode ConsoleTokenResponse: %v", err)
	}
	if parsed.Token == "" {
		return zero, fmt.Errorf("agentclient: ConsoleTokenResponse.token is empty")
	}
	return parsed, nil
}

// postVMLifecycleAsync is the shared engine for the four async
// lifecycle ops (start / stop / poweroff / reboot). The agent endpoint
// shape is identical — POST, no body, 202 returns AsyncTaskAccepted —
// so the dispatch sites differ only in the URL action segment.
func (c *Client) postVMLifecycleAsync(
	ctx context.Context,
	endpoint, action, vmName, idempotencyKey string,
) (uuid.UUID, error) {
	target, _ := url.JoinPath(endpoint, "/v1/vms/"+vmName+"/"+action)
	req, err := newRequest(ctx, http.MethodPost, target, nil)
	if err != nil {
		return uuid.Nil, err
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	_, body, err := c.do(req)
	if err != nil {
		return uuid.Nil, fmt.Errorf("agentclient: %s vm: %w", action, err)
	}
	var accepted agentapi.AsyncTaskAccepted
	if err := json.Unmarshal(body, &accepted); err != nil {
		return uuid.Nil, fmt.Errorf("agentclient: decode AsyncTaskAccepted after %s: %v", action, err)
	}
	if accepted.TaskID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("agentclient: AsyncTaskAccepted.task_id is zero uuid after %s", action)
	}
	return accepted.TaskID, nil
}

// DeleteVM submits an async delete request to the agent and returns
// the agent's task id from the AsyncTaskAccepted envelope. 202 is the
// happy path; 404 surfaces as *AgentError (caller decides whether to
// treat absent VMs as idempotent — typically yes for retry loops).
// The agent's `DELETE /v1/vms/{vm_name}` is
// name-keyed; callers pass the VM name (the CP-side loader hands
// the worker `vm.Name` together with `vm.ID` so the executor still
// has both for projection).
func (c *Client) DeleteVM(
	ctx context.Context,
	endpoint string,
	vmName string,
	idempotencyKey string,
) (uuid.UUID, error) {
	target, _ := url.JoinPath(endpoint, "/v1/vms/"+vmName)
	req, err := newRequest(ctx, http.MethodDelete, target, nil)
	if err != nil {
		return uuid.Nil, err
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}

	_, body, err := c.do(req)
	if err != nil {
		return uuid.Nil, fmt.Errorf("agentclient: delete vm: %w", err)
	}

	var accepted agentapi.AsyncTaskAccepted
	if err := json.Unmarshal(body, &accepted); err != nil {
		return uuid.Nil, fmt.Errorf("agentclient: decode AsyncTaskAccepted: %v", err)
	}
	if accepted.TaskID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("agentclient: AsyncTaskAccepted.task_id is zero uuid")
	}
	return accepted.TaskID, nil
}

// StartVMOnTarget starts the migrated guest on the target node after a
// committed cutover. It is the post-cutover convergence wrapper the migration
// worker calls best-effort: the returned agent task id is discarded (the worker
// does not poll it), only a non-nil error matters so the worker can log the
// leak. Thin wrapper over StartVM.
func (c *Client) StartVMOnTarget(ctx context.Context, endpoint, vmName string) error {
	_, err := c.StartVM(ctx, endpoint, vmName, "")
	return err
}

// DeleteVMOnSource deletes the source's now-stale copy after a committed
// cutover. Best-effort post-cutover convergence wrapper: the agent task id is
// discarded; a non-nil error signals a leaked source disk, never a destroyed
// wanted VM (this is only ever called after the cutover committed). Thin
// wrapper over DeleteVM.
//
// A 404 not_found from the agent is treated as success: after a successful
// outgoing-live migration the source agent tears its own departed VM down before
// reporting completion, so the CP's slower async delete usually arrives to a VM
// that is already gone. That is exactly the idempotent already-deleted outcome we
// want, not a leaked disk - swallowing it avoids a false "disk leaked" WARN. Any
// other agent error still propagates so a real leak is logged.
func (c *Client) DeleteVMOnSource(ctx context.Context, endpoint, vmName string) error {
	_, err := c.DeleteVM(ctx, endpoint, vmName, "")
	var ae *AgentError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return nil
	}
	return err
}

// NudgeHeartbeat triggers an immediate heartbeat on the agent at endpoint after
// an overlay cutover. Thin wrapper over PostHeartbeatNudge that lets
// *Client satisfy migrations.MigrationAgentClient structurally; best-effort, a
// non-nil error is logged by the worker, never fails the committed migration.
func (c *Client) NudgeHeartbeat(ctx context.Context, endpoint string) error {
	return c.PostHeartbeatNudge(ctx, endpoint)
}
