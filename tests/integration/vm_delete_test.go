// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package integration_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentmock"
	"github.com/otherix/otherix/internal/store"
)

// TestVMDelete_VerticalSliceHappyPath drives the full delete chain:
// vm.create runs first (so the row exists), then vm.delete fires the
// async pipeline through к the soft-delete InTx (DeleteVMRuntime +
// SoftDeleteVMDisksByVM + SoftDeleteVM + DecrementTemplateDerivedVMCount).
func TestVMDelete_VerticalSliceHappyPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplateForVM(t, ctx, v, v.adminID, "vm-del", 0xd9, "private")

	vmName := "del-vm-" + uuid.NewString()[:8]
	createTaskID, _ := v.createVM(t, ctx, vmCreateBody{
		Name:     vmName,
		Template: tpl.Name,
		Pool:     v.pool.ID.String(),
		VCPUs:    1,
		MemoryMB: 512,
	}, "")
	v.awaitVMCreateEvent(t, 15*time.Second)

	createRow, err := v.store.Queries().GetTask(ctx, createTaskID)
	if err != nil {
		t.Fatalf("GetTask create: %v", err)
	}
	if createRow.Status != store.TaskStatusSuccess {
		t.Fatalf("create task not success: %q (error=%s)", createRow.Status, string(createRow.Error))
	}
	vmID := extractVMIDFromTask(t, createRow)

	// Pre-load mock с a delete-success outcome — per Pre-L1 Path D
	// the agentmock keys vm.delete results by name.
	stageVMDeleteSuccess(v, vmName, 30*time.Millisecond)

	// Fire delete.
	deleteStatus, deleteBody := v.deleteVMRequest(t, ctx, vmID, "", "")
	if deleteStatus != http.StatusAccepted {
		t.Fatalf("vm delete status = %d, body = %s, want 202", deleteStatus, deleteBody)
	}

	v.awaitVMDeleteEvent(t, 15*time.Second)

	// vms.deleted_at set; subsequent GetVMByID returns ErrNoRows
	// (soft-delete excluded by `where deleted_at is null`).
	if _, err := v.store.Queries().GetVMByID(ctx, vmID); err == nil {
		t.Errorf("GetVMByID after soft-delete returned a row; want ErrNoRows")
	}

	// vm_runtime row was removed by the worker's projectDeleteSuccess InTx.
	if _, err := v.store.Queries().GetVMRuntime(ctx, vmID); err == nil {
		t.Errorf("vm_runtime row still present after delete; want gone")
	}

	// derived_vm_count decremented in the same InTx.
	tplAfter, err := v.store.Queries().GetTemplate(ctx, tpl.ID)
	if err != nil {
		t.Fatalf("GetTemplate: %v", err)
	}
	if tplAfter.DerivedVmCount != 0 {
		t.Errorf("template.derived_vm_count = %d, want 0 (one create + one delete)", tplAfter.DerivedVmCount)
	}

	// agentmock observed the vm.delete; storedVMs cleared.
	if got := v.agentVMDeleteCallCount(); got < 1 {
		t.Errorf("agent.vms.delete calls = %d, want ≥ 1", got)
	}
	if _, present := v.mock.GetStoredVM(vmName); present {
		t.Errorf("mock storedVM still present after agent delete success")
	}
}

// TestVMDelete_VerticalSliceAgentFailure injects а 500 on the agent's
// vm.delete; the worker classifies the error и marks the task failed.
// Crucially the vm row stays — operators can re-issue delete или
// inspect.
func TestVMDelete_VerticalSliceAgentFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplateForVM(t, ctx, v, v.adminID, "vm-del-fail", 0xea, "private")

	createTaskID, _ := v.createVM(t, ctx, vmCreateBody{
		Name:     "del-fail-vm-" + uuid.NewString()[:8],
		Template: tpl.Name,
		Pool:     v.pool.ID.String(),
		VCPUs:    1,
		MemoryMB: 512,
	}, "")
	v.awaitVMCreateEvent(t, 15*time.Second)
	createRow, _ := v.store.Queries().GetTask(ctx, createTaskID)
	vmID := extractVMIDFromTask(t, createRow)

	// Inject 500 on the next vm.delete agent call.
	v.mock.InjectError("vms.delete", agentmock.InjectedError{
		Status:  http.StatusInternalServerError,
		Code:    "internal",
		Message: "agent overloaded",
	})

	deleteStatus, _ := v.deleteVMRequest(t, ctx, vmID, "", "")
	if deleteStatus != http.StatusAccepted {
		t.Fatalf("vm delete enqueue status = %d, want 202", deleteStatus)
	}
	ev := v.awaitVMDeleteEvent(t, 15*time.Second)
	_ = ev

	// vms row still present (soft-delete только on success).
	if _, err := v.store.Queries().GetVMByID(ctx, vmID); err != nil {
		t.Errorf("GetVMByID after failed delete returned %v; want row present", err)
	}
}

// TestVMDelete_VerticalSliceCrossUserNotFound — developer attempts К
// delete admin's VM. CheckOwnership returns ErrPermissionDenied →
// handler maps К 404 (no leak).
func TestVMDelete_VerticalSliceCrossUserNotFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplateForVM(t, ctx, v, v.adminID, "vm-del-rbac", 0xfb, "private")

	// Admin creates a VM.
	createTaskID, _ := v.createVM(t, ctx, vmCreateBody{
		Name:     "rbac-del-vm-" + uuid.NewString()[:8],
		Template: tpl.Name,
		Pool:     v.pool.ID.String(),
		VCPUs:    1,
		MemoryMB: 512,
	}, "")
	v.awaitVMCreateEvent(t, 15*time.Second)
	createRow, _ := v.store.Queries().GetTask(ctx, createTaskID)
	vmID := extractVMIDFromTask(t, createRow)

	// Developer тries к delete it. Developer holds vm:delete=own; the
	// admin-owned VM matches neither the own-scope branch nor the
	// any-scope branch, so CheckOwnership rejects → handler maps к 404.
	_, devEmail, devPW := seedUser(t, ctx, v.store, "developer")
	devToken := loginUser(t, v.cpServer.URL, devEmail, devPW)

	status, body := v.deleteVMRequest(t, ctx, vmID, devToken, "")
	if status != http.StatusNotFound {
		t.Errorf("cross-user delete status = %d, body = %s, want 404 (no leak)", status, body)
	}
}
