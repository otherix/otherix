// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/agentclient"
)

// agentVMLifecycleClient is the narrow agent-client surface the L2
// async executor depends on. *agentclient.Client satisfies it
// structurally; tests substitute an in-test fake without importing
// the production client.
type agentVMLifecycleClient interface {
	StartVM(ctx context.Context, endpoint, vmName, idempotencyKey string) (uuid.UUID, error)
	StopVM(ctx context.Context, endpoint, vmName, idempotencyKey string) (uuid.UUID, error)
	PoweroffVM(ctx context.Context, endpoint, vmName, idempotencyKey string) (uuid.UUID, error)
	RebootVM(ctx context.Context, endpoint, vmName, idempotencyKey string) (uuid.UUID, error)
	PollTask(ctx context.Context, endpoint string, agentTaskID uuid.UUID) (agentclient.TaskTerminal, error)
}

// agentVMLifecycleExecutor is the production LifecycleExecutor: POST
// to the agent's per-op endpoint, persist the agent task id, then
// PollTask. agent_task_id resumption mirrors the create / delete
// executor — when args.AgentTaskID is non-nil the POST is skipped и
// the executor goes straight к polling.
type agentVMLifecycleExecutor struct {
	client agentVMLifecycleClient
}

// NewAgentVMLifecycleExecutor returns the production
// LifecycleExecutor. The same instance handles all four ops; the
// per-op dispatch lives inside Execute.
func NewAgentVMLifecycleExecutor(client agentVMLifecycleClient) LifecycleExecutor {
	return &agentVMLifecycleExecutor{client: client}
}

// Execute implements LifecycleExecutor. Drives one of four
// agentclient methods based on op, then polls the resulting agent
// task к terminal status.
func (e *agentVMLifecycleExecutor) Execute(ctx context.Context, op string, args LifecycleArgs) (LifecycleResult, error) {
	agentTaskID, err := e.postOrResume(ctx, op, args)
	if err != nil {
		return LifecycleResult{}, err
	}
	terminal, err := e.client.PollTask(ctx, args.Node.AdvertisedEndpoint, agentTaskID)
	if err != nil {
		return LifecycleResult{}, fmt.Errorf("poll task: %w", err)
	}
	switch terminal.Status {
	case "success":
		return LifecycleResult{VMID: args.VM.ID.String()}, nil
	case "failed", "cancelled":
		if terminal.Error != nil {
			return LifecycleResult{}, terminal.Error
		}
		return LifecycleResult{}, fmt.Errorf("agent task %s without error envelope", terminal.Status)
	default:
		return LifecycleResult{}, fmt.Errorf("unexpected agent terminal status %q", terminal.Status)
	}
}

// postOrResume returns the agent task id к poll. On а fresh attempt
// it dispatches the appropriate agent endpoint and persists the
// returned id; on resumption it echoes the supplied id back without
// contacting the agent.
func (e *agentVMLifecycleExecutor) postOrResume(ctx context.Context, op string, args LifecycleArgs) (uuid.UUID, error) {
	if args.AgentTaskID != nil {
		return *args.AgentTaskID, nil
	}
	idemKey := uuid.NewString()
	agentTaskID, err := e.dispatch(ctx, op, args.Node.AdvertisedEndpoint, args.VM.Name, idemKey)
	if err != nil {
		return uuid.Nil, fmt.Errorf("post vm.%s: %w", op, err)
	}
	if args.OnAgentTaskID == nil {
		return uuid.Nil, errors.New("LifecycleArgs.OnAgentTaskID is nil; cannot persist agent_task_id")
	}
	if err := args.OnAgentTaskID(ctx, agentTaskID); err != nil {
		return uuid.Nil, fmt.Errorf("persist agent_task_id: %v", err)
	}
	return agentTaskID, nil
}

func (e *agentVMLifecycleExecutor) dispatch(
	ctx context.Context, op, endpoint, vmName, idemKey string,
) (uuid.UUID, error) {
	switch op {
	case "start":
		return e.client.StartVM(ctx, endpoint, vmName, idemKey)
	case "stop":
		return e.client.StopVM(ctx, endpoint, vmName, idemKey)
	case "poweroff":
		return e.client.PoweroffVM(ctx, endpoint, vmName, idemKey)
	case "reboot":
		return e.client.RebootVM(ctx, endpoint, vmName, idemKey)
	default:
		return uuid.Nil, fmt.Errorf("unknown async lifecycle op %q", op)
	}
}
