// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/store"
)

// This file holds the queue contract for the async VM tasks:
// the job-arg payloads (with their Kind), the per-task executor seams + their
// argument/result shapes, the terminal error-code catalogue, and the shared
// error helpers. The worker implementations that consume this contract live in
// run.go (the etcd job runtime); the executors are realised by
// agent_executors*.go in production and the Stub* doubles in tests.

// VMCreateArgs is the queue job-args payload for a `vm.create` task. The
// atomic-enqueue handler (Create) inserts the task row, mints a fresh task id,
// and enqueues this payload; the worker resolves vm / pool / node before
// dispatching to the executor. The image source lives on the self-describing
// VM / disk rows, so the worker reads it from there rather than from the job
// payload - only the row identifiers are carried here.
type VMCreateArgs struct {
	TaskID uuid.UUID `json:"task_id"`
	VMID   uuid.UUID `json:"vm_id"`
	PoolID uuid.UUID `json:"pool_id"`
	NodeID uuid.UUID `json:"node_id"`
	// SourceSnapshotID is the snapshot the VM is recreated from
	// (`vm create --from-snapshot`), or nil for an image-sourced VM. The agent
	// worker (slice A Task 12) materializes the VM disks from the snapshot blobs
	// when this is set. Threaded from the VM row at bind time.
	SourceSnapshotID *uuid.UUID `json:"source_snapshot_id,omitempty"`
}

// Kind names the job kind. Mirrors the OpenAPI Task.type value surfaced through
// tasks.{list,get}.
func (VMCreateArgs) Kind() string { return "vm.create" }

// VMDeleteArgs is the queue job-args payload for a `vm.delete` task.
type VMDeleteArgs struct {
	TaskID uuid.UUID `json:"task_id"`
	VMID   uuid.UUID `json:"vm_id"`
	NodeID uuid.UUID `json:"node_id"`
}

// Kind names the job kind.
func (VMDeleteArgs) Kind() string { return "vm.delete" }

// VMStartArgs / VMStopArgs / VMPoweroffArgs / VMRebootArgs are the job-args
// payloads for the L2 async lifecycle pipeline. The worker re-loads the VM +
// node before dispatching to the executor - same pattern as VMCreateArgs /
// VMDeleteArgs.
type VMStartArgs struct {
	TaskID uuid.UUID `json:"task_id"`
	VMID   uuid.UUID `json:"vm_id"`
	NodeID uuid.UUID `json:"node_id"`
}

// Kind names the job kind.
func (VMStartArgs) Kind() string { return "vm.start" }

// VMStopArgs is the job-args payload for a `vm.stop` task.
type VMStopArgs struct {
	TaskID uuid.UUID `json:"task_id"`
	VMID   uuid.UUID `json:"vm_id"`
	NodeID uuid.UUID `json:"node_id"`
}

// Kind names the job kind.
func (VMStopArgs) Kind() string { return "vm.stop" }

// VMPoweroffArgs is the job-args payload for a `vm.poweroff` task.
type VMPoweroffArgs struct {
	TaskID uuid.UUID `json:"task_id"`
	VMID   uuid.UUID `json:"vm_id"`
	NodeID uuid.UUID `json:"node_id"`
}

// Kind names the job kind.
func (VMPoweroffArgs) Kind() string { return "vm.poweroff" }

// VMRebootArgs is the job-args payload for a `vm.reboot` task.
type VMRebootArgs struct {
	TaskID uuid.UUID `json:"task_id"`
	VMID   uuid.UUID `json:"vm_id"`
	NodeID uuid.UUID `json:"node_id"`
}

// Kind names the job kind.
func (VMRebootArgs) Kind() string { return "vm.reboot" }

// CreateArgs is the per-task input the create executor receives. AgentTaskID
// carries the persisted agent-side task uuid for resumption - nil on first run,
// non-nil when the worker is recovering from a CP-side restart.
//
// OnAgentTaskID is the callback the executor invokes immediately after the
// agent's 202 returns, persisting the agent task id via UpdateTaskAgentTaskID.
// The executor never touches the store directly.
type CreateArgs struct {
	TaskID      uuid.UUID
	AgentTaskID *uuid.UUID
	VM          store.VM
	Disk        store.VMDisk
	// ImageURL / ImageSHA256 / Format / DiskGiB are the image source the
	// executor hands to the agent. ImageSHA256 is the hex-encoded digest
	// (empty when unpinned). The worker reads these off the self-describing VM
	// and disk rows.
	ImageURL      string
	ImageSHA256   string
	Format        string
	DiskGiB       int
	Pool          store.StoragePool
	Node          store.Node
	NICs          []agentclient.VMCreateNIC
	OnAgentTaskID func(ctx context.Context, agentTaskID uuid.UUID) error
}

// CreateResult is the create executor's output. Surfaced into the task's
// `result` JSONB so operators can correlate without a follow-up GET. The agent
// resolves the content digest and the materialized/virtual disk sizes; the
// worker echoes them into the task result.
type CreateResult struct {
	VMID             string `json:"vm_id"`
	ImageSHA256      string `json:"image_sha256"`
	DiskSizeBytes    int64  `json:"disk_size_bytes"`
	VirtualSizeBytes int64  `json:"virtual_size_bytes"`
}

// CreateExecutor is the per-task-type seam for vm.create. Production
// implementation is agentVMCreateExecutor over the agentclient package; tests
// use StubVMCreateExecutor.
type CreateExecutor interface {
	Execute(ctx context.Context, args CreateArgs) (CreateResult, error)
}

// DeleteArgs / DeleteResult / DeleteExecutor mirror the create seam for the
// vm.delete pipeline. The agent's `DELETE /v1/vms/{vm_name}` is name-keyed; the
// worker loads the VM row before dispatch so both the CP-side UUID (for the
// result projection) and the agent-facing name are available.
type DeleteArgs struct {
	TaskID        uuid.UUID
	AgentTaskID   *uuid.UUID
	VMID          uuid.UUID
	VMName        string
	Node          store.Node
	OnAgentTaskID func(ctx context.Context, agentTaskID uuid.UUID) error
}

// DeleteResult is the delete executor's output.
type DeleteResult struct {
	VMID string `json:"vm_id"`
}

// DeleteExecutor is the per-task-type seam for vm.delete. Production
// implementation is agentVMDeleteExecutor; tests use StubVMDeleteExecutor.
type DeleteExecutor interface {
	Execute(ctx context.Context, args DeleteArgs) (DeleteResult, error)
}

// LifecycleArgs is the per-task input the async lifecycle executors receive.
// Shared by all four L2 operations because the agent-side dispatch shape
// (endpoint, vmName, idempotency key) is identical. AgentTaskID carries the
// persisted agent-side task uuid for resumption.
//
// OnAgentTaskID is the callback the executor invokes immediately after the
// agent's 202 returns, persisting the agent task id. Same shape as CreateArgs /
// DeleteArgs.
type LifecycleArgs struct {
	TaskID        uuid.UUID
	AgentTaskID   *uuid.UUID
	VM            store.VM
	Node          store.Node
	OnAgentTaskID func(ctx context.Context, agentTaskID uuid.UUID) error
}

// LifecycleResult is the executor's output.
type LifecycleResult struct {
	VMID string `json:"vm_id"`
}

// LifecycleExecutor is the per-task-type seam for vm.start / vm.stop /
// vm.poweroff / vm.reboot. Production implementation lives in
// agent_executors_lifecycle.go; tests pass a stub. The op string is one of
// "start" / "stop" / "poweroff" / "reboot" - drives the agent-side endpoint
// selection.
type LifecycleExecutor interface {
	Execute(ctx context.Context, op string, args LifecycleArgs) (LifecycleResult, error)
}

// Failure code constants for the task `error` envelope. Pass-through agent
// codes (`qemu_spawn_failed`, `image_unavailable`, `checksum_mismatch`,
// `pool_full`, ...) are preserved verbatim by classifyVMError.
const (
	errCodeVMNotFound         = "vm_not_found"
	errCodeVMPoolMissing      = "pool_not_found"
	errCodeVMNodeMissing      = "node_not_found"
	errCodeVMAgentUnreachabl  = "agent_unreachable"
	errCodeVMTimeout          = "request_timeout"
	errCodeVMCreateFailed     = "vm_create_failed"
	errCodeVMDeleteFailed     = "vm_delete_failed"
	errCodeVMImageUnavailable = "image_unavailable"
	errCodeVMChecksumMismatch = "checksum_mismatch"
)

// Failure codes specific to the L2 async lifecycle surface. Pass-through agent
// codes (stop_timeout, qemu_supervision_failed, qmp_unavailable, ...) are
// preserved verbatim by classifyVMError.
const (
	errCodeVMStartFailed    = "vm_start_failed"
	errCodeVMStopFailed     = "vm_stop_failed"
	errCodeVMPoweroffFailed = "vm_poweroff_failed"
	errCodeVMRebootFailed   = "vm_reboot_failed"
)

// marshalTaskError serialises a (code, message) pair into the JSONB payload the
// OpenAPI Task.error field surfaces.
func marshalTaskError(code, message string) ([]byte, error) {
	return json.Marshal(struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: message})
}

// classifyVMError maps an executor error to a tasks.error.code. Agent envelope
// passthrough preserves agent-side codes verbatim; network / 5xx failures
// collapse to agent_unreachable; poll-budget exhaustion to request_timeout;
// everything else falls through to the supplied fallback (vm_create_failed /
// vm_delete_failed / ...).
func classifyVMError(err error, fallback string) string {
	var ae *agentclient.AgentError
	if errors.As(err, &ae) {
		if ae.Code != "" {
			return ae.Code
		}
		return errCodeVMAgentUnreachabl
	}
	var te *agentclient.TimeoutError
	if errors.As(err, &te) {
		return errCodeVMTimeout
	}
	return fallback
}
