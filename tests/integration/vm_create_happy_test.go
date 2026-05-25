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

	"github.com/otherix/otherix/internal/store"
)

// TestVMCreate_VerticalSliceHappyPath drives the full Phase B vm.create
// chain end-to-end: handler → real river → real worker (production
// agentVMCreateExecutor) → functional mock-agent (Phase A vm
// endpoints) → poll → finalize → vm_runtime row + derived_vm_count
// projection.
func TestVMCreate_VerticalSliceHappyPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplateForVM(t, ctx, v, v.adminID, "vm-happy", 0xa1, "private")

	// The CP mints the VM uuid inside the handler — we cannot pre-stage
	// per-uuid against the agentmock. The default success path (empty
	// queue → default success) covers the create.
	taskID, _ := v.createVM(t, ctx, vmCreateBody{
		Name:     "happy-vm-" + uuid.NewString()[:8],
		Template: tpl.Name,
		Pool:     v.pool.ID.String(),
		VCPUs:    2,
		MemoryMB: 2048,
	}, "")

	v.awaitVMCreateEvent(t, 15*time.Second)

	row, err := v.store.Queries().GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if row.Status != store.TaskStatusSuccess {
		t.Fatalf("task.Status = %q, want success (error=%s)", row.Status, string(row.Error))
	}
	if row.AgentTaskID == nil {
		t.Error("task.AgentTaskID is nil after success — resumption surface broken")
	}
	vmID := extractVMIDFromTask(t, row)

	// Phase B worker upserts vm_runtime с phase=running on success.
	rt, err := v.store.Queries().GetVMRuntime(ctx, vmID)
	if err != nil {
		t.Fatalf("GetVMRuntime: %v", err)
	}
	if rt.Phase != store.VmPhaseRunning {
		t.Errorf("vm_runtime.phase = %q, want running", rt.Phase)
	}
	if rt.CurrentNodeID == nil || *rt.CurrentNodeID != v.node.ID {
		t.Errorf("vm_runtime.current_node_id = %v, want %s", rt.CurrentNodeID, v.node.ID)
	}

	// derived_vm_count incremented в the same InTx as task finalization.
	tplAfter, err := v.store.Queries().GetTemplate(ctx, tpl.ID)
	if err != nil {
		t.Fatalf("GetTemplate (after): %v", err)
	}
	if tplAfter.DerivedVmCount != 1 {
		t.Errorf("template.derived_vm_count = %d, want 1", tplAfter.DerivedVmCount)
	}

	// vm_disks row was inserted at handler time (atomic enqueue) и
	// carries the storage_pool_id reference per Phase A D3.
	disks, err := v.store.Queries().ListVMDisksByVM(ctx, vmID)
	if err != nil {
		t.Fatalf("ListVMDisksByVM: %v", err)
	}
	if len(disks) != 1 {
		t.Fatalf("vm_disks = %d, want 1", len(disks))
	}
	if disks[0].StoragePoolID != v.pool.ID {
		t.Errorf("vm_disk.storage_pool_id = %s, want %s", disks[0].StoragePoolID, v.pool.ID)
	}

	// agentmock holds the materialised AgentVM after terminal-success.
	// Per Pre-L1 Path D the inventory is keyed by VM name; resolve via
	// the CP row to locate the entry.
	createdVM, err := v.store.Queries().GetVMByID(ctx, vmID)
	if err != nil {
		t.Fatalf("GetVMByID for mock lookup: %v", err)
	}
	stored, ok := v.mock.StoredVM(createdVM.Name)
	if !ok {
		t.Fatalf("mock.StoredVM(%q) = false; want VM materialised after terminal-success", createdVM.Name)
	}
	if stored.ID != vmID || stored.PoolName != v.pool.Name {
		t.Errorf("mock storedVM = %+v, want id=%s pool=%s", stored, vmID, v.pool.Name)
	}

	// task.result carries the vm_id projection — single round-trip
	// observable to operators без a follow-up GET.
	var result struct {
		VMID string `json:"vm_id"`
	}
	if err := json.Unmarshal(row.Result, &result); err != nil {
		t.Fatalf("unmarshal task.result: %v", err)
	}
	if result.VMID != vmID.String() {
		t.Errorf("task.result.vm_id = %q, want %q", result.VMID, vmID)
	}

	// CP-side projection through GET /v1/vms/{id} surfaces status=running.
	getStatus, getBody := v.getVMRequest(t, ctx, vmID, "")
	if getStatus != 200 {
		t.Fatalf("GET /v1/vms/{id} = %d, body = %s, want 200", getStatus, getBody)
	}
	if !jsonContains(getBody, `"status":"running"`) {
		t.Errorf("vm get body missing status=running: %s", getBody)
	}
}
