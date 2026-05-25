// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/store"
)

// resolveCloudInitUserData merges а VM-level override against the
// template-level default per L3 Area 3 lock — explicit disable wins,
// else VM wins когда set, else template's cloud_init_user_data wins,
// else empty (agent skips cidata generation). Both source columns are
// tri-state nullable *string в the store; we treat NULL и "" identically
// as "absent" so the operator can pass either form interchangeably.
//
// Three-state semantic (operator UX iteration):
//
//   - vm.CloudInitDisabled=true                  → "" (explicit disable)
//   - vm.UserData set                            → override
//   - template.CloudInitUserData set             → fallback
//   - none of the above                          → "" (no cloud-init)
//
// The DB CHECK chk_vms_cloud_init_disabled_no_userdata makes the
// disabled-AND-userdata combination unreachable; the early return
// here is а defense-in-depth signal к the reader, not а runtime branch.
func resolveCloudInitUserData(vm store.VM, tpl store.Template) string {
	if vm.CloudInitDisabled {
		return ""
	}
	if vm.UserData != nil && *vm.UserData != "" {
		return *vm.UserData
	}
	if tpl.CloudInitUserData != nil {
		return *tpl.CloudInitUserData
	}
	return ""
}

// agentVMClient is the narrow agent-client surface vm executors
// depend on. *agentclient.Client satisfies it structurally; tests
// pass an in-test fake without importing the production client.
// Per Pre-L1 Path D the agent's per-VM URLs are name-keyed; the
// DeleteVM signature carries the name string the executor reads
// from `DeleteArgs.VMName`.
type agentVMClient interface {
	PostVMCreate(
		ctx context.Context,
		endpoint string,
		idempotencyKey string,
		req agentclient.VMCreateRequest,
	) (uuid.UUID, error)
	DeleteVM(
		ctx context.Context,
		endpoint string,
		vmName string,
		idempotencyKey string,
	) (uuid.UUID, error)
	PollTask(
		ctx context.Context,
		endpoint string,
		agentTaskID uuid.UUID,
	) (agentclient.TaskTerminal, error)
}

// agentVMCreateExecutor is the production CreateExecutor: PostVMCreate
// then PollTask, with agent_task_id resumption.
//
// Resumption: when args.AgentTaskID is non-nil, the executor skips
// the POST and goes straight to PollTask - a CP-side restart
// mid-poll picks back up against the same agent_task_id without
// duplicating the qemu spawn.
type agentVMCreateExecutor struct {
	client agentVMClient
}

// NewAgentVMCreateExecutor returns the production CreateExecutor.
func NewAgentVMCreateExecutor(client agentVMClient) CreateExecutor {
	return &agentVMCreateExecutor{client: client}
}

// Execute implements CreateExecutor.
func (e *agentVMCreateExecutor) Execute(ctx context.Context, args CreateArgs) (CreateResult, error) {
	agentTaskID, err := e.postOrResumeCreate(ctx, args)
	if err != nil {
		return CreateResult{}, err
	}
	terminal, err := e.client.PollTask(ctx, args.Node.AdvertisedEndpoint, agentTaskID)
	if err != nil {
		return CreateResult{}, fmt.Errorf("poll task: %w", err)
	}
	switch terminal.Status {
	case "success":
		return CreateResult{VMID: args.VM.ID.String()}, nil
	case "failed", "cancelled":
		if terminal.Error != nil {
			return CreateResult{}, terminal.Error
		}
		return CreateResult{}, fmt.Errorf("agent task %s without error envelope", terminal.Status)
	default:
		return CreateResult{}, fmt.Errorf("unexpected agent terminal status %q", terminal.Status)
	}
}

// postOrResumeCreate returns the agent task id to poll. On a fresh
// attempt (args.AgentTaskID nil) it issues the agent POST and
// invokes OnAgentTaskID with the returned id; on a resumption it
// echoes the supplied id back without contacting the agent.
func (e *agentVMCreateExecutor) postOrResumeCreate(ctx context.Context, args CreateArgs) (uuid.UUID, error) {
	if args.AgentTaskID != nil {
		return *args.AgentTaskID, nil
	}
	idemKey := uuid.NewString()
	body := agentclient.VMCreateRequest{
		UUID:             args.VM.ID,
		Name:             args.VM.Name,
		VCPUs:            int(args.VM.CpuCores),
		MemoryMB:         int(args.VM.MemoryMib),
		Pool:             args.Pool.Name,
		TemplateChecksum: templateChecksumHex(args.Template),
		UserData:         resolveCloudInitUserData(args.VM, args.Template),
	}
	agentTaskID, err := e.client.PostVMCreate(ctx, args.Node.AdvertisedEndpoint, idemKey, body)
	if err != nil {
		return uuid.Nil, fmt.Errorf("post vm.create: %w", err)
	}
	if args.OnAgentTaskID == nil {
		return uuid.Nil, errors.New("CreateArgs.OnAgentTaskID is nil; cannot persist agent_task_id")
	}
	if err := args.OnAgentTaskID(ctx, agentTaskID); err != nil {
		return uuid.Nil, fmt.Errorf("persist agent_task_id: %v", err)
	}
	return agentTaskID, nil
}

// agentVMDeleteExecutor is the production DeleteExecutor: DeleteVM
// then PollTask с the same resumption pattern.
type agentVMDeleteExecutor struct {
	client agentVMClient
}

// NewAgentVMDeleteExecutor returns the production DeleteExecutor.
func NewAgentVMDeleteExecutor(client agentVMClient) DeleteExecutor {
	return &agentVMDeleteExecutor{client: client}
}

// Execute implements DeleteExecutor.
func (e *agentVMDeleteExecutor) Execute(ctx context.Context, args DeleteArgs) (DeleteResult, error) {
	agentTaskID, err := e.postOrResumeDelete(ctx, args)
	if err != nil {
		// Idempotent-success short-circuit: agent reported 404 on
		// the POST, the VM is already gone — surface as success
		// without polling.
		if errors.Is(err, errAgentVMAlreadyGone) {
			return DeleteResult{VMID: args.VMID.String()}, nil
		}
		return DeleteResult{}, err
	}
	terminal, err := e.client.PollTask(ctx, args.Node.AdvertisedEndpoint, agentTaskID)
	if err != nil {
		return DeleteResult{}, fmt.Errorf("poll task: %w", err)
	}
	switch terminal.Status {
	case "success":
		return DeleteResult{VMID: args.VMID.String()}, nil
	case "failed", "cancelled":
		if terminal.Error != nil {
			// 404 from the agent on delete is treated as idempotent
			// success — the VM is already gone, the user-visible
			// outcome is what they asked for. Other error codes
			// surface verbatim.
			if terminal.Error.Status == 404 {
				return DeleteResult{VMID: args.VMID.String()}, nil
			}
			return DeleteResult{}, terminal.Error
		}
		return DeleteResult{}, fmt.Errorf("agent task %s without error envelope", terminal.Status)
	default:
		return DeleteResult{}, fmt.Errorf("unexpected agent terminal status %q", terminal.Status)
	}
}

func (e *agentVMDeleteExecutor) postOrResumeDelete(ctx context.Context, args DeleteArgs) (uuid.UUID, error) {
	if args.AgentTaskID != nil {
		return *args.AgentTaskID, nil
	}
	idemKey := uuid.NewString()
	agentTaskID, err := e.client.DeleteVM(ctx, args.Node.AdvertisedEndpoint, args.VMName, idemKey)
	if err != nil {
		// 404 on delete-of-missing-vm is idempotent success — the
		// agent doesn't have the VM, which is the desired outcome.
		var ae *agentclient.AgentError
		if errors.As(err, &ae) && ae.Status == 404 {
			return uuid.Nil, errAgentVMAlreadyGone
		}
		return uuid.Nil, fmt.Errorf("delete vm: %w", err)
	}
	if args.OnAgentTaskID == nil {
		return uuid.Nil, errors.New("DeleteArgs.OnAgentTaskID is nil; cannot persist agent_task_id")
	}
	if err := args.OnAgentTaskID(ctx, agentTaskID); err != nil {
		return uuid.Nil, fmt.Errorf("persist agent_task_id: %v", err)
	}
	return agentTaskID, nil
}

// errAgentVMAlreadyGone is the in-flight signal that the agent
// reported 404 on delete — handled as idempotent success at the
// Execute layer above.
var errAgentVMAlreadyGone = errors.New("agent reported vm already gone")
