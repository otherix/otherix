// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package integration_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	vmshandlers "github.com/otherix/otherix/internal/api/handlers/vms"
	"github.com/otherix/otherix/internal/store"
)

// TestVMCreate_VerticalSliceResumption verifies the agent_task_id
// resumption surface (the same pattern the storage-image import
// worker uses, reused in VMCreateWorker). The test drives:
//
//  1. A real first vm.create attempt (full happy path) so the
//     agentmock task uuid and the CP-side tasks.agent_task_id linkage
//     get established.
//
//  2. The vm_runtime row is removed to simulate "projection lost
//     between agent commit and CP finalize" — same Window B shape
//     the storage_image.import resumption test exercises. A second
//     tasks row is created with the SAME agent_task_id pre-populated
//     (status=running), simulating a CP-side restart mid-poll.
//
// Worker.Work is invoked directly (bypasses river retry) with the new
// task id. The production agentVMCreateExecutor observes
// args.AgentTaskID != nil and skips PostVMCreate, going straight to
// PollTask. The mock task projection still resolves to terminal-
// success on the second poll, so the worker re-projects via
// UpsertVMRuntime + IncrementTemplateDerivedVMCount.
//
// Empirical assertion: agent saw exactly ONE vms.create call across
// both attempts. A regression where the executor re-POSTs on resume
// would surface as count = 2.
func TestVMCreate_VerticalSliceResumption(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplateForVM(t, ctx, v, v.adminID, "vm-resume", 0xe5, "private")

	// First attempt - real vm.create end-to-end.
	firstTaskID, _ := v.createVM(t, ctx, vmCreateBody{
		Name:     "resume-vm-" + uuid.NewString()[:8],
		Template: tpl.Name,
		Pool:     v.pool.ID.String(),
		VCPUs:    2,
		MemoryMB: 1024,
	}, "")
	v.awaitVMCreateEvent(t, 15*time.Second)

	firstRow, err := v.store.Queries().GetTask(ctx, firstTaskID)
	if err != nil {
		t.Fatalf("GetTask first: %v", err)
	}
	if firstRow.Status != store.TaskStatusSuccess {
		t.Fatalf("first task.Status = %q, want success (error=%s)", firstRow.Status, string(firstRow.Error))
	}
	if firstRow.AgentTaskID == nil {
		t.Fatal("first task.AgentTaskID is nil — resumption seam not exercised")
	}
	agentTaskID := *firstRow.AgentTaskID
	vmID := extractVMIDFromTask(t, firstRow)

	if got := v.agentVMCreateCallCount(); got != 1 {
		t.Fatalf("after first attempt, agent.vms.create calls = %d, want 1", got)
	}

	// Simulate "projection lost": drop the vm_runtime row. The vms +
	// vm_disks rows stay (the resumption flow runs against them as if
	// the worker is recovering mid-flight).
	if err := v.store.Queries().DeleteVMRuntime(ctx, vmID); err != nil {
		t.Fatalf("DeleteVMRuntime: %v", err)
	}

	// Second attempt - fresh tasks row carrying the EXISTING agent_task_id.
	freshTaskID := uuid.New()
	resID := vmID
	creatorID := v.adminID
	args, err := json.Marshal(map[string]any{
		"vm_id":       vmID.String(),
		"template_id": tpl.ID.String(),
		"pool_id":     v.pool.ID.String(),
		"node_id":     v.node.ID.String(),
	})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	if _, err := v.store.Queries().CreateTask(ctx, store.CreateTaskParams{
		ID:           freshTaskID,
		Type:         "vm.create",
		Status:       store.TaskStatusPending,
		ResourceType: "vm",
		ResourceID:   &resID,
		Args:         args,
		MaxAttempts:  25,
		CreatedBy:    &creatorID,
	}); err != nil {
		t.Fatalf("CreateTask fresh: %v", err)
	}
	if err := v.store.Queries().UpdateTaskAgentTaskID(ctx, store.UpdateTaskAgentTaskIDParams{
		ID:          freshTaskID,
		AgentTaskID: &agentTaskID,
	}); err != nil {
		t.Fatalf("UpdateTaskAgentTaskID: %v", err)
	}

	// Drive Worker.Work directly — bypasses river so the test does
	// not have to mutate river_job rows.
	deps := vmshandlers.CreateDeps{
		Store:    v.store,
		Executor: vmshandlers.NewAgentVMCreateExecutor(v.agentClient),
		Logger:   v.logger,
	}
	worker := vmshandlers.NewVMCreateWorker(deps)
	job := &river.Job[vmshandlers.VMCreateArgs]{
		Args: vmshandlers.VMCreateArgs{
			TaskID:     freshTaskID,
			VMID:       vmID,
			TemplateID: tpl.ID,
			PoolID:     v.pool.ID,
			NodeID:     v.node.ID,
		},
	}
	if err := worker.Work(ctx, job); err != nil {
		t.Fatalf("worker.Work resumption: %v", err)
	}

	freshRow, err := v.store.Queries().GetTask(ctx, freshTaskID)
	if err != nil {
		t.Fatalf("GetTask fresh: %v", err)
	}
	if freshRow.Status != store.TaskStatusSuccess {
		t.Fatalf("fresh task.Status = %q, want success (error=%s)", freshRow.Status, string(freshRow.Error))
	}

	// vm_runtime re-projected.
	if _, err := v.store.Queries().GetVMRuntime(ctx, vmID); err != nil {
		t.Fatalf("vm_runtime row not re-created after resumption: %v", err)
	}

	// Critical: agent saw exactly ONE vm.create POST across the two
	// attempts. The resumption path skipped the second POST.
	if got := v.agentVMCreateCallCount(); got != 1 {
		t.Errorf("after resumption, agent.vms.create calls = %d, want 1 (resumption must skip POST)", got)
	}
}
