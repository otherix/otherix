// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"context"
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

// NOTE: the delete-racing-bind seam test (a CP-side delete of an unscheduled VM
// concurrent with a reconcile tick must never resurrect the VM or enqueue a
// task) lands with Task 10, which introduces DeleteUnscheduledVM. Double-bind
// safety on its own is already covered by the etcdstore BindScheduledVM CAS test
// (Task 4) and the idempotent second-tick assertion above.
