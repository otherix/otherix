// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// Coverage split (case b - the apie2e harness builds the real chi router over
// etcdstore but does NOT run the worker Dispatcher, so async ops are only
// asserted up to enqueue here; see the dispatcher-less assertions in
// vms_schedule_test.go for the same pattern). This file therefore proves the
// HTTP contract apie2e owns:
//
//   - POST /v1/vms/{name}/migrate returns 202 + a pending vm.migrate task and
//     atomically creates a migration row visible via GET /v1/migrations/{id} in
//     phase=pending.
//   - POST /v1/migrations/{id}/cancel returns 200 with the row in
//     phase=cancelled, and the VM stays pinned to its source (cancel is
//     fail-safe-to-source, never touches PinnedNodeID).
//   - The explicit-target no-op path: migrate with target_node == the VM's
//     current pinned node returns 200 already_on_target and creates no migration.
//
// The worker-drains-to-cutover proof (happy nodeless lifecycle, the
// single-eligible-node pending path with scheduling_reason=no_eligible_target,
// pre-cutover failure, redelivery without double-POST) lives in
// internal/api/handlers/migrations/run_test.go, which drives the real worker
// entry against an agentmock, plus the Lima real-agent smoke. scheduling_reason
// is set by run.go and cannot surface here without a dispatcher draining the
// job, so it is intentionally NOT asserted in apie2e.

// migrationView is the apie2e projection of GET /v1/migrations/{id} - only the
// fields the contract tests assert on.
type migrationView struct {
	ID               string  `json:"id"`
	VMID             string  `json:"vm_id"`
	Phase            string  `json:"phase"`
	SourceNodeID     *string `json:"source_node_id"`
	TargetNodeID     *string `json:"target_node_id"`
	SchedulingReason *string `json:"scheduling_reason"`
}

// asyncAccepted is the 202 AsyncTaskAccepted envelope.
type asyncAccepted struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
}

// seedScheduledVM seeds a scheduled VM pinned to a freshly-created ready node,
// owned by ownerID. Unlike seedMigratableVM (which seeds an UNscheduled,
// node-less VM for the "leave this node" intent) this one sets PinnedNodeID so
// the explicit-target no-op path can be exercised. The migrate POST only reads
// PinnedNodeID off the resolved VM, so no vm_disks row is needed.
func seedScheduledVM(t *testing.T, s *etcdstore.Store, ownerID uuid.UUID) (pinnedNodeID uuid.UUID, vmName string) {
	t.Helper()
	ctx := context.Background()
	node, err := s.CreateNode(ctx, store.CreateNodeParams{
		ID:                      uuid.New(),
		Name:                    "pin-node-" + uuid.NewString()[:8],
		Architecture:            store.CpuArchAmd64,
		AdvertisedEndpoint:      "https://node:9443",
		MigrationHost:           "10.0.0.3",
		MigrationPortRangeStart: 49152,
		MigrationPortRangeEnd:   49251,
		Status:                  store.NodeStatusReady,
	})
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	spec, err := store.MarshalSchedulingSpec(store.SchedulingSpec{PoolName: "default"})
	if err != nil {
		t.Fatalf("MarshalSchedulingSpec: %v", err)
	}
	pinned := node.ID
	vm := store.VM{
		ID: uuid.New(), OwnerID: ownerID, Name: "pin-vm-" + uuid.NewString()[:8],
		DesiredPhase: store.VmDesiredPhaseRunning, Architecture: store.CpuArchAmd64,
		SchedulingStatus: store.VMSchedulingScheduled, SchedulingSpec: spec,
		PinnedNodeID: &pinned,
		CpuCores:     2, MemoryMib: 2048,
		Labels: []byte(`{}`), ImageFormat: store.ImageFormatQcow2,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	cli := sharedEtcdClient
	if err := cli.PutJSON(ctx, etcd.Key("vms", vm.ID.String()), vm); err != nil {
		t.Fatalf("seed vm: %v", err)
	}
	if err := cli.Put(ctx, etcd.Key("uniq", "vms", "name", vm.Name), []byte(vm.ID.String())); err != nil {
		t.Fatalf("seed vm name guard: %v", err)
	}
	return node.ID, vm.Name
}

// migrationIDForVM resolves the (single) migration created for vmID via the
// store, so the test can poll GET /v1/migrations/{id} without depending on the
// 202 envelope carrying the migration id (it carries only the task id).
func migrationIDForVM(t *testing.T, s *etcdstore.Store, vmID uuid.UUID) uuid.UUID {
	t.Helper()
	rows, err := s.ListMigrations(context.Background(), store.ListMigrationsParams{
		VmID: &vmID, LimitCount: 10,
	})
	if err != nil {
		t.Fatalf("ListMigrations: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("migrations for vm %s = %d, want 1", vmID, len(rows))
	}
	return rows[0].ID
}

// TestMigrationLifecycle_HTTPContract drives the operator HTTP path apie2e owns:
// a node-less migrate creates a pending migration (202 + task), the migration is
// readable via GET /v1/migrations/{id} as pending with the source pinned and no
// target bound, and cancel flips it to cancelled while the VM stays on its
// source. (The worker drain to cutover is proven in run_test.go + the Lima
// smoke; see the file-level comment.)
func TestMigrationLifecycle_HTTPContract(t *testing.T) {
	h := newE2E(t)
	opToken, opID := loginAs(t, h, auth.RoleOperator)

	// seedMigratableVM seeds an UNscheduled VM (no PinnedNodeID): a node-less
	// migrate of it is the "leave this node" intent, so the migration's
	// source_node_id is nil (there is no current node to leave yet). The node it
	// returns is only a namable source, not a pin.
	_, vmName := seedMigratableVM(t, h.store, opID)
	vmID := vmIDByName(t, h.store, vmName)

	// Node-less migrate -> 202 + a pending vm.migrate task.
	resp := h.do(t, http.MethodPost, "/v1/vms/"+vmName+"/migrate", map[string]any{}, opToken, map[string]string{
		"Idempotency-Key": "mig-life-" + uuid.NewString(),
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("migrate status = %d, want 202", resp.StatusCode)
	}
	var accepted asyncAccepted
	decodeJSON(t, resp, &accepted)
	if accepted.Status != string(store.TaskStatusPending) {
		t.Errorf("accepted.status = %q, want pending", accepted.Status)
	}
	if accepted.TaskID == "" {
		t.Error("accepted.task_id is empty")
	}

	// The migration row exists and is readable via GET /v1/migrations/{id} as
	// pending, source pinned, no target bound (no dispatcher ran, so the worker
	// has not scheduled a target and scheduling_reason stays nil).
	migID := migrationIDForVM(t, h.store, vmID)
	var mig migrationView
	resp = h.get(t, "/v1/migrations/"+migID.String(), opToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get migration status = %d, want 200", resp.StatusCode)
	}
	decodeJSON(t, resp, &mig)
	if mig.Phase != string(store.MigrationPhasePending) {
		t.Errorf("migration phase = %q, want pending", mig.Phase)
	}
	if mig.SourceNodeID != nil {
		t.Errorf("source_node_id = %v, want nil (unscheduled VM has no current node to leave)", *mig.SourceNodeID)
	}
	if mig.TargetNodeID != nil {
		t.Errorf("target_node_id = %v, want nil (no target bound without a dispatcher)", *mig.TargetNodeID)
	}
	if mig.SchedulingReason != nil {
		t.Errorf("scheduling_reason = %q, want nil (set by run.go, no dispatcher here)", *mig.SchedulingReason)
	}

	// Cancel -> 200 cancelled.
	resp = h.do(t, http.MethodPost, "/v1/migrations/"+migID.String()+"/cancel", nil, opToken, map[string]string{
		"Idempotency-Key": "mig-cancel-" + uuid.NewString(),
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel status = %d, want 200", resp.StatusCode)
	}
	var cancelled migrationView
	decodeJSON(t, resp, &cancelled)
	if cancelled.Phase != string(store.MigrationPhaseCancelled) {
		t.Errorf("cancelled phase = %q, want cancelled", cancelled.Phase)
	}

	// The VM stays pinned to its source - cancel never touches PinnedNodeID.
	vmAfter, err := h.store.VMByID(context.Background(), vmID)
	if err != nil {
		t.Fatalf("VMByID after cancel: %v", err)
	}
	if vmAfter.PinnedNodeID != nil {
		t.Errorf("vm pinned_node_id = %v after cancel, want nil (unscheduled source preserved)", *vmAfter.PinnedNodeID)
	}
}

// TestMigrate_AlreadyOnTarget_NoOp locks the explicit-target no-op short-circuit
// (start.go resolveTargetNode, spec Resolved-decision 3): migrating to the VM's
// current pinned node returns 200 already_on_target and creates NO migration row.
func TestMigrate_AlreadyOnTarget_NoOp(t *testing.T) {
	h := newE2E(t)
	opToken, opID := loginAs(t, h, auth.RoleOperator)

	pinnedNodeID, vmName := seedScheduledVM(t, h.store, opID)
	vmID := vmIDByName(t, h.store, vmName)
	pinnedNode, err := h.store.NodeByID(context.Background(), pinnedNodeID)
	if err != nil {
		t.Fatalf("NodeByID: %v", err)
	}

	resp := h.do(t, http.MethodPost, "/v1/vms/"+vmName+"/migrate",
		map[string]any{"target_node": pinnedNode.Name}, opToken, map[string]string{
			"Idempotency-Key": "mig-noop-" + uuid.NewString(),
		})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("no-op migrate status = %d, want 200", resp.StatusCode)
	}
	var noop struct {
		Status        string `json:"status"`
		CurrentNodeID string `json:"current_node_id"`
	}
	decodeJSON(t, resp, &noop)
	if noop.Status != "already_on_target" {
		t.Errorf("status = %q, want already_on_target", noop.Status)
	}
	if noop.CurrentNodeID != pinnedNodeID.String() {
		t.Errorf("current_node_id = %q, want %q", noop.CurrentNodeID, pinnedNodeID)
	}

	// No migration row was created by the no-op.
	rows, err := h.store.ListMigrations(context.Background(), store.ListMigrationsParams{
		VmID: &vmID, LimitCount: 10,
	})
	if err != nil {
		t.Fatalf("ListMigrations: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("migrations after no-op = %d, want 0", len(rows))
	}
}
