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

// seedMigratableVM seeds an unscheduled (pending) VM owned by ownerID, plus a
// ready node it can name as a migration source. The VM is intentionally
// unscheduled so the read path renders without a vm_disks row (an unscheduled
// VM falls back to the SchedulingSpec); the migrate handler accepts it (a
// node-less migration is the "leave this node" intent). It writes the VM
// primary and its name guard directly.
func seedMigratableVM(t *testing.T, s *etcdstore.Store, ownerID uuid.UUID) (sourceNodeID uuid.UUID, vmName string) {
	t.Helper()
	ctx := context.Background()
	node, err := s.CreateNode(ctx, store.CreateNodeParams{
		ID:                      uuid.New(),
		Name:                    "rt-node-" + uuid.NewString()[:8],
		Architecture:            store.CpuArchAmd64,
		AdvertisedEndpoint:      "https://node:9443",
		MigrationHost:           "10.0.0.1",
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
	vm := store.VM{
		ID: uuid.New(), OwnerID: ownerID, Name: "rt-vm-" + uuid.NewString()[:8],
		DesiredPhase: store.VmDesiredPhaseRunning, Architecture: store.CpuArchAmd64,
		SchedulingStatus: store.VMSchedulingUnscheduled, SchedulingSpec: spec,
		CpuCores: 2, MemoryMib: 2048,
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

// TestMigrationRoutes_Wiring locks T16's route wiring + RBAC gating end to end
// through the real router: operator migrate -> 202; developer migrate -> 403
// (RequirePermission(vm:migrate) fires before the handler, so a developer who
// lacks the capability is rejected at the middleware regardless of ownership);
// viewer GET /v1/migrations -> 200 (Get/List under vm:read so a VM viewer can
// poll).
func TestMigrationRoutes_Wiring(t *testing.T) {
	h := newE2E(t)

	opToken, opID := loginAs(t, h, auth.RoleOperator)
	devToken, _ := loginAs(t, h, auth.RoleDeveloper)
	viewerToken, _ := loginAs(t, h, auth.RoleViewer)

	// Seed a VM owned by the operator so the migrate handler resolves it.
	_, vmName := seedMigratableVM(t, h.store, opID)

	// Operator migrate -> 202 Accepted (route + handler + store all wired).
	resp := h.do(t, http.MethodPost, "/v1/vms/"+vmName+"/migrate", map[string]any{}, opToken, map[string]string{
		"Idempotency-Key": "mig-route-" + uuid.NewString(),
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("operator migrate status = %d, want 202", resp.StatusCode)
	}
	resp.Body.Close()

	// Developer migrate -> 403 at the RequirePermission(vm:migrate) gate. The
	// developer lacks the capability entirely, so the middleware rejects before
	// the handler runs (no ownership/404 path reached). VM name need not even
	// exist for this gate.
	resp = h.do(t, http.MethodPost, "/v1/vms/"+vmName+"/migrate", map[string]any{}, devToken, map[string]string{
		"Idempotency-Key": "mig-route-dev-" + uuid.NewString(),
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("developer migrate status = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	// Viewer GET /v1/migrations -> 200 (vm:read gates the read path).
	resp = h.get(t, "/v1/migrations", viewerToken)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("viewer list migrations status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
}

// vmStatusMigrationView captures only status.migration of the VM projection so
// the seam test can assert its presence/absence and shape.
type vmStatusMigrationView struct {
	Node   *string `json:"node"`
	Status struct {
		Phase     string `json:"phase"`
		Migration *struct {
			ID              string  `json:"id"`
			Phase           string  `json:"phase"`
			ProgressPercent int     `json:"progress_percent"`
			TargetNodeID    *string `json:"target_node_id"`
		} `json:"migration"`
	} `json:"status"`
}

// TestVMStatusMigration_Surface locks T16's status.migration seam through the
// real GET /v1/vms/{name} path: absent with no migration, populated (compact
// {id, phase, progress_percent, target_node_id}) once a migration is active,
// and the VM's current node (rendered as `node`) is never flipped to the target
// by the summary - current_node stays = source until cutover.
func TestVMStatusMigration_Surface(t *testing.T) {
	h := newE2E(t)
	admin, adminID := loginAs(t, h, auth.RoleAdmin)
	ctx := context.Background()

	sourceNodeID, vmName := seedMigratableVM(t, h.store, adminID)
	vmID := vmIDByName(t, h.store, vmName)

	// No migration yet -> status.migration absent.
	var before vmStatusMigrationView
	resp := h.get(t, "/v1/vms/"+vmName, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get vm status = %d, want 200", resp.StatusCode)
	}
	decodeJSON(t, resp, &before)
	if before.Status.Migration != nil {
		t.Errorf("status.migration = %+v, want nil (no active migration)", before.Status.Migration)
	}

	// Seed a node-bound active migration directly through the store.
	target, err := h.store.CreateNode(ctx, store.CreateNodeParams{
		ID: uuid.New(), Name: "tgt-" + uuid.NewString()[:8], Architecture: store.CpuArchAmd64,
		AdvertisedEndpoint: "https://tgt:9443", MigrationHost: "10.0.0.2",
		MigrationPortRangeStart: 49152, MigrationPortRangeEnd: 49251, Status: store.NodeStatusReady,
	})
	if err != nil {
		t.Fatalf("CreateNode(target): %v", err)
	}
	src := sourceNodeID
	tgt := target.ID
	migID := uuid.New()
	taskID := uuid.New()
	if _, err := h.store.CreateMigration(ctx, store.CreateMigrationParams{
		ID: migID, VmID: vmID, SourceNodeID: &src, TargetNodeID: &tgt,
		Reason: store.MigrationReasonManual, Live: true,
		Task: store.CreateTaskParams{
			ID: taskID, Type: "vm.migrate", Status: store.TaskStatusPending,
			ResourceType: "migration", MaxAttempts: 3,
		},
	}, routeJobArgsStub{}); err != nil {
		t.Fatalf("CreateMigration: %v", err)
	}

	var after vmStatusMigrationView
	resp = h.get(t, "/v1/vms/"+vmName, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get vm status = %d, want 200", resp.StatusCode)
	}
	decodeJSON(t, resp, &after)
	if after.Status.Migration == nil {
		t.Fatalf("status.migration = nil, want summary")
	}
	if after.Status.Migration.ID != migID.String() {
		t.Errorf("status.migration.id = %q, want %q", after.Status.Migration.ID, migID)
	}
	if after.Status.Migration.Phase != string(store.MigrationPhasePending) {
		t.Errorf("status.migration.phase = %q, want pending", after.Status.Migration.Phase)
	}
	if after.Status.Migration.TargetNodeID == nil || *after.Status.Migration.TargetNodeID != tgt.String() {
		t.Errorf("status.migration.target_node_id = %v, want %q", after.Status.Migration.TargetNodeID, tgt)
	}
	// current_node (node) must still resolve to the source, not the target.
	if after.Node != nil && *after.Node == target.Name {
		t.Errorf("vm node = %q (target), want source - current_node must not flip until cutover", *after.Node)
	}
}

// vmIDByName resolves a seeded VM name to its id via the store.
func vmIDByName(t *testing.T, s *etcdstore.Store, name string) uuid.UUID {
	t.Helper()
	vm, err := s.VMByName(context.Background(), name)
	if err != nil {
		t.Fatalf("VMByName(%q): %v", name, err)
	}
	return vm.ID
}

// routeJobArgsStub is a minimal queue.JobArgs for seeding a migration's backing
// job in the route/seam tests.
type routeJobArgsStub struct{}

func (routeJobArgsStub) Kind() string { return "vm.migrate" }
