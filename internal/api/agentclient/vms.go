// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
)

// VMCreateRequest is the body the CP-side worker sends to the agent's
// POST /v1/vms. Wire-shape matches the Iteration 1 agent's actual
// surface (internal/agent/handlers/vms/create.go) — minimal MVP fields
// only. The richer agent.yaml VMSpec is a future design target;
// reconciliation between the two is tracked as future iteration work.
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
	UUID             uuid.UUID `json:"uuid"`
	Name             string    `json:"name"`
	VCPUs            int       `json:"vcpus"`
	MemoryMB         int       `json:"memory_mb"`
	Pool             string    `json:"pool"`
	TemplateChecksum string    `json:"template_checksum"`
	// UserData carries the CP-resolved cloud-init `#cloud-config`
	// blob (L3 Area 3 lock — already-merged vm.user_data ?:
	// template.cloud_init_user_data, hostname-injected). Empty
	// string on the wire means "no cidata"; the agent skips
	// ISO generation when absent.
	UserData string `json:"user_data,omitempty"`
	// Nics are the fully-resolved network interfaces to attach. The
	// CP-side worker resolves each vm_nic row against its network
	// (bridge_name + mtu come from the network; mac/model/order from
	// the vm_nic). Absent or empty means legacy SLIRP user-mode
	// networking on the agent.
	Nics []VMCreateNIC `json:"nics,omitempty"`
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
}

// agentVMListResponse mirrors the agent's listResponse wire shape
// (handler.go list.go). The meta envelope is forward-compatible with
// cursor pagination but Iteration 1 agent leaves next_cursor null.
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
// user-facing scope; the CP-side worker mints a fresh per-attempt
// key (resumption pattern keyed by agent_task_id).
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

// ListVMs returns the agent's full VM inventory. Iteration 1 agent
// returns a single page without pagination; future iterations may layer
// on cursor pagination — this method's signature stays the same since
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
// envelope contract as PauseVM. Per Pre-L1 spec amendment reset is
// sync (200 + VMDetail) rather than the previous 202 + task.
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
// Per Pre-L1 Path D the path is name-keyed (`/v1/vms/{vm_name}/...`).
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
// Per Pre-L1 Path D the agent's `DELETE /v1/vms/{vm_name}` is
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
