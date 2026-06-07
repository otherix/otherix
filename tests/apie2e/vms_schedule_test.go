// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/handlers/vms"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/scheduler"
	"github.com/otherix/otherix/internal/store"
)

// scheduleLogger returns a slog logger discarding all output, for the
// reconcile-loop seam tests.
func scheduleLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// defaultScheduleResources returns the zero-value resources config: every
// dimension disabled, so scoring degrades to count-based. The seam tests use
// the least_vm_count algorithm, which never consults resource fit, so the
// fixture's metric-free node still binds.
func defaultScheduleResources() scheduler.ResourcesConfig {
	return scheduler.ResourcesConfig{}
}

// createPendingVM drives the real POST /v1/vms admission path (-> 201 pending)
// and returns the created VM id. Seam tests use it so the scheduler runs over a
// VM written by the real handler, not a direct store call.
func createPendingVM(t *testing.T, h *harness, admin, poolName string) uuid.UUID {
	t.Helper()
	body := vmCreateBody(map[string]any{"pool": poolName})
	resp := h.post(t, "/v1/vms", body, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create vm status = %d, want 201", resp.StatusCode)
	}
	var created struct {
		ID     string `json:"id"`
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	decodeJSON(t, resp, &created)
	if created.Status.Phase != "pending" {
		t.Fatalf("created phase = %q, want pending", created.Status.Phase)
	}
	id, err := uuid.Parse(created.ID)
	if err != nil {
		t.Fatalf("parse created id %q: %v", created.ID, err)
	}
	return id
}

// hasVMCreateTask reports whether a vm.create task exists for the VM.
func hasVMCreateTask(t *testing.T, h *harness, vmID uuid.UUID) bool {
	t.Helper()
	typ := "vm.create"
	tasks, err := h.store.ListTasksAny(context.Background(), store.ListTasksAnyParams{
		TypeFilter:       &typ,
		ResourceIDFilter: &vmID,
		LimitCount:       100,
	})
	if err != nil {
		t.Fatalf("ListTasksAny: %v", err)
	}
	return len(tasks) > 0
}

// TestScheduleFunc_BindsWhenPoolReady drives the REAL reconcile sequence
// (vms.ScheduleFunc -> scheduler.SchedulePlacement -> BindScheduledVM): a VM
// created pending against a ready local_dir pool is bound to the node, flipped
// to scheduled, and a vm.create task is enqueued. It deliberately does NOT call
// BindScheduledVM directly - the seam under test is the loop body.
func TestScheduleFunc_BindsWhenPoolReady(t *testing.T) {
	h := newE2E(t)
	ctx := context.Background()
	admin, adminID := loginAs(t, h, auth.RoleAdmin)
	nodeID, poolName := schedulableFixtureWithNode(t, h, adminID)

	vmID := createPendingVM(t, h, admin, poolName)

	fn := vms.ScheduleFunc(h.store, vms.ScheduleConfig{Algorithm: "least_vm_count"}, scheduleLogger(), defaultScheduleResources())
	if err := fn(ctx); err != nil {
		t.Fatalf("ScheduleFunc: %v", err)
	}

	vm, err := h.store.VMByID(ctx, vmID)
	if err != nil {
		t.Fatalf("VMByID: %v", err)
	}
	if vm.SchedulingStatus != store.VMSchedulingScheduled {
		t.Fatalf("SchedulingStatus = %q, want scheduled", vm.SchedulingStatus)
	}
	if vm.PinnedNodeID == nil || *vm.PinnedNodeID != nodeID {
		t.Errorf("PinnedNodeID = %v, want %v", vm.PinnedNodeID, nodeID)
	}
	if vm.SchedulingReason != nil {
		t.Errorf("SchedulingReason = %v, want nil after bind", vm.SchedulingReason)
	}
	if !hasVMCreateTask(t, h, vmID) {
		t.Error("no vm.create task enqueued after bind")
	}

	// A second tick is a no-op: the VM is already scheduled (dropped from the
	// unscheduled index), so the loop neither re-binds nor duplicates the task.
	if err := fn(ctx); err != nil {
		t.Fatalf("ScheduleFunc (second tick): %v", err)
	}
	vm2, err := h.store.VMByID(ctx, vmID)
	if err != nil {
		t.Fatalf("VMByID (second tick): %v", err)
	}
	if vm2.SchedulingStatus != store.VMSchedulingScheduled {
		t.Errorf("SchedulingStatus after second tick = %q, want scheduled", vm2.SchedulingStatus)
	}
	typ := "vm.create"
	tasks, err := h.store.ListTasksAny(ctx, store.ListTasksAnyParams{
		TypeFilter: &typ, ResourceIDFilter: &vmID, LimitCount: 100,
	})
	if err != nil {
		t.Fatalf("ListTasksAny: %v", err)
	}
	if len(tasks) != 1 {
		t.Errorf("vm.create tasks after two ticks = %d, want 1 (idempotent)", len(tasks))
	}
}

// TestScheduleFunc_PendingReasonWhenPoolMissing drives the loop against a VM
// whose pool does not exist: admission defers, the VM is pending, and the
// reconcile tick records reason=pool_not_found while leaving the VM unscheduled
// (no failure transition - retry forever).
func TestScheduleFunc_PendingReasonWhenPoolMissing(t *testing.T) {
	h := newE2E(t)
	ctx := context.Background()
	admin, _ := loginAs(t, h, auth.RoleAdmin)
	// Seed a default firmware so admission resolves firmware, but no pool named
	// "nope" exists - the create stays pending.
	seedDefaultFirmware(t, h)

	vmID := createPendingVM(t, h, admin, "nope")

	fn := vms.ScheduleFunc(h.store, vms.ScheduleConfig{Algorithm: "least_vm_count"}, scheduleLogger(), defaultScheduleResources())
	if err := fn(ctx); err != nil {
		t.Fatalf("ScheduleFunc: %v", err)
	}

	vm, err := h.store.VMByID(ctx, vmID)
	if err != nil {
		t.Fatalf("VMByID: %v", err)
	}
	if vm.SchedulingStatus != store.VMSchedulingUnscheduled {
		t.Errorf("SchedulingStatus = %q, want unscheduled", vm.SchedulingStatus)
	}
	if vm.SchedulingReason == nil || *vm.SchedulingReason != store.SchedReasonPoolNotFound {
		t.Errorf("SchedulingReason = %v, want %q", vm.SchedulingReason, store.SchedReasonPoolNotFound)
	}
	if hasVMCreateTask(t, h, vmID) {
		t.Error("vm.create task enqueued for a still-pending VM, want none")
	}

	// Re-GET through the public API surfaces the reason on status.reason.
	getResp := h.get(t, "/v1/vms/"+vm.Name, admin)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("get vm status = %d, want 200", getResp.StatusCode)
	}
	var got struct {
		Status struct {
			Phase  string `json:"phase"`
			Reason string `json:"reason"`
		} `json:"status"`
	}
	decodeJSON(t, getResp, &got)
	if got.Status.Phase != "pending" || got.Status.Reason != store.SchedReasonPoolNotFound {
		t.Errorf("public status = %+v, want phase=pending reason=%q", got.Status, store.SchedReasonPoolNotFound)
	}
}

// TestVMDelete_PendingVMSyncNoContent drives DELETE /v1/vms/{name} on a PENDING
// (unscheduled) VM: with no node and no agent task, the CP deletes it directly
// and returns 204 No Content (not the scheduled-VM 202 + vm.delete task). A
// subsequent GET is 404, and no vm.delete task is enqueued.
func TestVMDelete_PendingVMSyncNoContent(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)
	// Seed a default firmware so admission resolves firmware, but no pool named
	// "nope" exists - the VM stays pending (unscheduled).
	seedDefaultFirmware(t, h)

	body := vmCreateBody(map[string]any{"pool": "nope"})
	vmName := body["name"].(string)
	createResp := h.post(t, "/v1/vms", body, admin)
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create vm status = %d, want 201", createResp.StatusCode)
	}
	var created struct {
		ID     string `json:"id"`
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	decodeJSON(t, createResp, &created)
	if created.Status.Phase != "pending" {
		t.Fatalf("created phase = %q, want pending", created.Status.Phase)
	}
	vmID, err := uuid.Parse(created.ID)
	if err != nil {
		t.Fatalf("parse created id %q: %v", created.ID, err)
	}

	delResp := h.delete(t, "/v1/vms/"+vmName, admin)
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete pending vm status = %d, want 204", delResp.StatusCode)
	}

	getResp := h.get(t, "/v1/vms/"+vmName, admin)
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusNotFound {
		t.Errorf("get after delete status = %d, want 404", getResp.StatusCode)
	}

	// No vm.delete task was enqueued (the delete was CP-side only).
	delType := "vm.delete"
	delTasks, err := h.store.ListTasksAny(context.Background(), store.ListTasksAnyParams{
		TypeFilter: &delType, ResourceIDFilter: &vmID, LimitCount: 100,
	})
	if err != nil {
		t.Fatalf("ListTasksAny: %v", err)
	}
	if len(delTasks) != 0 {
		t.Errorf("vm.delete tasks = %d, want 0 (pending delete is CP-side)", len(delTasks))
	}
}

// TestScheduleFunc_DeletedPendingVMNotResurrected is the delete-vs-bind seam test
// (deferred from Task 9): a pending VM deleted CP-side, then a reconcile tick,
// must stay GONE and never enqueue a vm.create task. It proves the scheduler loop
// fail-closes against a deleted VM - DeleteUnscheduledVM drops the unscheduled
// index and the VM row, so the next ListUnscheduledVMs cannot observe it (and
// even on an index-lag observation the bind CAS would lose against the missing
// row). The VM is created against a READY pool, so the only thing keeping the
// scheduler from binding it is the delete - a regression that resurrected the row
// would immediately bind it and enqueue the task this asserts is absent.
func TestScheduleFunc_DeletedPendingVMNotResurrected(t *testing.T) {
	h := newE2E(t)
	ctx := context.Background()
	admin, adminID := loginAs(t, h, auth.RoleAdmin)
	_, poolName := schedulableFixtureWithNode(t, h, adminID)

	vmID := createPendingVM(t, h, admin, poolName)

	// Delete it CP-side (204) before any reconcile tick runs.
	body := h.delete(t, "/v1/vms/"+vmNameOf(t, h, admin, vmID), admin)
	body.Body.Close()
	if body.StatusCode != http.StatusNoContent {
		t.Fatalf("delete pending vm status = %d, want 204", body.StatusCode)
	}

	// Now run the reconcile loop once: the deleted VM must not come back.
	fn := vms.ScheduleFunc(h.store, vms.ScheduleConfig{Algorithm: "least_vm_count"}, scheduleLogger(), defaultScheduleResources())
	if err := fn(ctx); err != nil {
		t.Fatalf("ScheduleFunc: %v", err)
	}

	if _, err := h.store.VMByID(ctx, vmID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("VMByID after delete+tick err = %v, want ErrNotFound (no resurrection)", err)
	}
	if hasVMCreateTask(t, h, vmID) {
		t.Error("vm.create task enqueued for a deleted VM - the scheduler resurrected it")
	}
}

// vmNameOf resolves a VM's name from its id via the public GET-by-id-less path is
// not available, so it reads the name from the store directly (the e2e harness
// shares the store). Keeps the delete-by-name call in the resurrection test
// honest without re-deriving the generated name.
func vmNameOf(t *testing.T, h *harness, _ string, id uuid.UUID) string {
	t.Helper()
	vm, err := h.store.VMByID(context.Background(), id)
	if err != nil {
		t.Fatalf("VMByID(%v): %v", id, err)
	}
	return vm.Name
}
