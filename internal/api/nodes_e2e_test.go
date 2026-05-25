// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// nodeBody builds a valid POST /v1/nodes payload. The migration port
// range is fixed at 49152-49251 (the production default);
// hostname is parameterised to avoid collisions across test cases.
func nodeBody(name string) map[string]any {
	return map[string]any{
		"name":                       name,
		"architecture":               "amd64",
		"advertised_endpoint":        "https://" + name + ".lab:9443",
		"migration_host":             "10.0.0.1",
		"migration_port_range_start": 49152,
		"migration_port_range_end":   49251,
	}
}

// uniqueName produces a node name that won't collide between tests
// sharing the Postgres container.
func uniqueName(prefix string) string {
	return prefix + "-" + uuid.NewString()[:8]
}

// createNodeAsAdmin seeds a node via the API and returns its operator-
// facing name (the `name` argument). `/v1/nodes/{name}` is name-only -
// call sites that build path URLs use the returned value directly.
// SQL-direct fixtures (status flip, vm_runtime seed, etc.) that need
// the row UUID call lookupNodeID to resolve via the store.
func createNodeAsAdmin(t *testing.T, h *e2eHarness, name string) string {
	t.Helper()
	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.post(t, "/v1/nodes", nodeBody(name), adminToken)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed node: status = %d, want 201", resp.StatusCode)
	}
	var v struct {
		ID string `json:"id"`
	}
	decodeJSON(t, resp, &v)
	if v.ID == "" {
		t.Fatal("seed node: empty id in response")
	}
	return name
}

// lookupNodeID fetches the row UUID for а node seeded via the API.
// Used by SQL-direct fixtures (status flip, vm_runtime seed) that need
// to address rows by primary key outside the public surface.
func lookupNodeID(t *testing.T, h *e2eHarness, name string) string {
	t.Helper()
	row, err := h.store.Queries().GetNodeByName(context.Background(), name)
	if err != nil {
		t.Fatalf("lookup node by name %q: %v", name, err)
	}
	return row.ID.String()
}

func TestE2E_Nodes_CreateRequiresAdmin(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	for _, role := range []auth.Role{auth.RoleOperator, auth.RoleDeveloper, auth.RoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			tok := loginAs(t, h, role)
			resp := h.post(t, "/v1/nodes", nodeBody(uniqueName("nope")), tok)
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", resp.StatusCode)
			}
		})
	}
}

func TestE2E_Nodes_CreateHappyPath(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	name := uniqueName("happy")
	resp := h.post(t, "/v1/nodes", nodeBody(name), adminToken)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	var view struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		Architecture string `json:"architecture"`
		Status       string `json:"status"`
		Migration    struct {
			Host           string `json:"host"`
			PortRangeStart int32  `json:"port_range_start"`
			PortRangeEnd   int32  `json:"port_range_end"`
		} `json:"migration"`
	}
	decodeJSON(t, resp, &view)
	if view.Name != name || view.Architecture != "amd64" || view.Status != "pending" {
		t.Errorf("view = %+v, want name=%q architecture=amd64 status=pending", view, name)
	}
	if view.Migration.Host != "10.0.0.1" || view.Migration.PortRangeStart != 49152 || view.Migration.PortRangeEnd != 49251 {
		t.Errorf("migration = %+v", view.Migration)
	}
	if _, err := uuid.Parse(view.ID); err != nil {
		t.Errorf("id = %q, not a uuid", view.ID)
	}
}

func TestE2E_Nodes_CreateValidation(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)

	cases := []struct {
		name string
		mut  func(map[string]any)
	}{
		{"empty name", func(b map[string]any) { b["name"] = "" }},
		{"bad architecture", func(b map[string]any) { b["architecture"] = "x86" }},
		{"empty advertised_endpoint", func(b map[string]any) { b["advertised_endpoint"] = "" }},
		{"port range end < start", func(b map[string]any) {
			b["migration_port_range_start"] = 50000
			b["migration_port_range_end"] = 49000
		}},
		{"port range below min", func(b map[string]any) {
			b["migration_port_range_start"] = 80
			b["migration_port_range_end"] = 90
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := nodeBody(uniqueName("v"))
			tc.mut(body)
			resp := h.post(t, "/v1/nodes", body, adminToken)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
			var b response.ErrorBody
			decodeJSON(t, resp, &b)
			if b.Error.Code != response.CodeValidationFailed {
				t.Errorf("code = %q, want %q", b.Error.Code, response.CodeValidationFailed)
			}
		})
	}
}

func TestE2E_Nodes_CreateDuplicateName(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	name := uniqueName("dup")
	resp1 := h.post(t, "/v1/nodes", nodeBody(name), adminToken)
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", resp1.StatusCode)
	}
	resp2 := h.post(t, "/v1/nodes", nodeBody(name), adminToken)
	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("dup status = %d, want 409", resp2.StatusCode)
	}
}

func TestE2E_Nodes_GetAndListShape(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	id := createNodeAsAdmin(t, h, uniqueName("shape"))

	adminToken := loginAs(t, h, auth.RoleAdmin)
	respFull := h.get(t, "/v1/nodes/"+id, adminToken)
	if respFull.StatusCode != http.StatusOK {
		t.Fatalf("admin get status = %d, want 200", respFull.StatusCode)
	}
	var full map[string]any
	decodeJSON(t, respFull, &full)
	for _, k := range []string{"advertised_endpoint", "migration", "capabilities", "cpu_flags"} {
		if _, ok := full[k]; !ok {
			t.Errorf("admin view missing %q (full Node expected)", k)
		}
	}

	devToken := loginAs(t, h, auth.RoleDeveloper)
	respSummary := h.get(t, "/v1/nodes/"+id, devToken)
	if respSummary.StatusCode != http.StatusOK {
		t.Fatalf("developer get status = %d, want 200", respSummary.StatusCode)
	}
	var summary map[string]any
	decodeJSON(t, respSummary, &summary)
	for _, k := range []string{"advertised_endpoint", "migration", "capabilities"} {
		if _, ok := summary[k]; ok {
			t.Errorf("developer view leaked %q (NodeSummary expected)", k)
		}
	}
	for _, k := range []string{"id", "name", "architecture", "status", "labels"} {
		if _, ok := summary[k]; !ok {
			t.Errorf("developer view missing %q", k)
		}
	}
}

func TestE2E_Nodes_GetUnknown404(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.get(t, "/v1/nodes/no-such-node-name", adminToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestE2E_Nodes_CordonRequiresMaintenance(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	id := createNodeAsAdmin(t, h, uniqueName("cordon-rbac"))

	for _, role := range []auth.Role{auth.RoleDeveloper, auth.RoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			tok := loginAs(t, h, role)
			resp := h.post(t, "/v1/nodes/"+id+"/cordon", nil, tok)
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want 403", resp.StatusCode)
			}
		})
	}

	operatorToken := loginAs(t, h, auth.RoleOperator)
	resp := h.post(t, "/v1/nodes/"+id+"/cordon", nil, operatorToken)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("operator cordon status = %d, want 200", resp.StatusCode)
	}
}

func TestE2E_Nodes_CordonUncordonHappyAndIdempotent(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	id := createNodeAsAdmin(t, h, uniqueName("cordon-flow"))
	adminToken := loginAs(t, h, auth.RoleAdmin)

	// pending → cordon → status=cordoned
	resp := h.post(t, "/v1/nodes/"+id+"/cordon", nil, adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cordon status = %d, want 200", resp.StatusCode)
	}
	var view struct {
		Status     string  `json:"status"`
		CordonedAt *string `json:"cordoned_at"`
	}
	decodeJSON(t, resp, &view)
	if view.Status != "cordoned" {
		t.Errorf("status after cordon = %q, want cordoned", view.Status)
	}
	if view.CordonedAt == nil || *view.CordonedAt == "" {
		t.Errorf("cordoned_at = %v, want timestamp", view.CordonedAt)
	}

	// cordon again → no-op 200, status unchanged
	resp = h.post(t, "/v1/nodes/"+id+"/cordon", nil, adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("repeat cordon status = %d, want 200 (no-op)", resp.StatusCode)
	}

	// cordoned → uncordon → status=ready, cordoned_at cleared
	resp = h.post(t, "/v1/nodes/"+id+"/uncordon", nil, adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("uncordon status = %d, want 200", resp.StatusCode)
	}
	decodeJSON(t, resp, &view)
	if view.Status != "ready" {
		t.Errorf("status after uncordon = %q, want ready", view.Status)
	}
	if view.CordonedAt != nil {
		t.Errorf("cordoned_at = %v, want nil after uncordon", view.CordonedAt)
	}

	// uncordon again → no-op 200
	resp = h.post(t, "/v1/nodes/"+id+"/uncordon", nil, adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("repeat uncordon status = %d, want 200", resp.StatusCode)
	}
}

func TestE2E_Nodes_UncordonInvalidTransition(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	id := createNodeAsAdmin(t, h, uniqueName("uncordon-bad"))
	adminToken := loginAs(t, h, auth.RoleAdmin)

	// fresh node is `pending`; uncordon must reject with 409.
	resp := h.post(t, "/v1/nodes/"+id+"/uncordon", nil, adminToken)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	if b.Error.Code != response.CodeConflict {
		t.Errorf("code = %q, want %q", b.Error.Code, response.CodeConflict)
	}
	if got, _ := b.Error.Details["current_status"].(string); got != "pending" {
		t.Errorf("current_status = %q, want pending", got)
	}
}

func TestE2E_Nodes_DeleteRequiresAdmin(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	id := createNodeAsAdmin(t, h, uniqueName("del-rbac"))

	for _, role := range []auth.Role{auth.RoleOperator, auth.RoleDeveloper, auth.RoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			tok := loginAs(t, h, role)
			resp := h.delete(t, "/v1/nodes/"+id, tok)
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want 403", resp.StatusCode)
			}
		})
	}
}

func TestE2E_Nodes_DeleteHappyPath(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	id := createNodeAsAdmin(t, h, uniqueName("del-happy"))
	adminToken := loginAs(t, h, auth.RoleAdmin)

	resp := h.delete(t, "/v1/nodes/"+id, adminToken)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}

	// Soft-deleted → GET 404.
	resp = h.get(t, "/v1/nodes/"+id, adminToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("post-delete get status = %d, want 404", resp.StatusCode)
	}
}

func TestE2E_Nodes_DeleteUnknown404(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.delete(t, "/v1/nodes/no-such-node-name", adminToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestE2E_Nodes_DeleteBlockedByVMRuntime(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	id := createNodeAsAdmin(t, h, uniqueName("del-blocked"))
	nodeID := uuid.MustParse(lookupNodeID(t, h, id))

	// Seed a VM runtime row pinned to this node.
	ownerID, _, _ := seedUserWithRole(t, ctx, h.store, auth.RoleDeveloper)
	seedVMOnNode(t, ctx, h.store, ownerID, nodeID)

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.delete(t, "/v1/nodes/"+id, adminToken)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	if b.Error.Code != response.CodeConflict {
		t.Errorf("code = %q, want %q", b.Error.Code, response.CodeConflict)
	}
	br, _ := b.Error.Details["blocking_resources"].(map[string]any)
	if br == nil {
		t.Fatalf("missing blocking_resources in details: %+v", b.Error.Details)
	}
	if vms, _ := br["vms"].(float64); vms < 1 {
		t.Errorf("blocking_resources.vms = %v, want ≥1", br["vms"])
	}
	if fa, _ := b.Error.Details["force_available"].(bool); !fa {
		t.Errorf("force_available = %v, want true", b.Error.Details["force_available"])
	}
}

func TestE2E_Nodes_ForceDeleteOrphansRuntime(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	id := createNodeAsAdmin(t, h, uniqueName("del-force"))
	nodeID := uuid.MustParse(lookupNodeID(t, h, id))

	ownerID, _, _ := seedUserWithRole(t, ctx, h.store, auth.RoleDeveloper)
	vmID := seedVMOnNode(t, ctx, h.store, ownerID, nodeID)

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.delete(t, "/v1/nodes/"+id+"?force=true", adminToken)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("force-delete status = %d, want 204", resp.StatusCode)
	}

	// vm_runtime row: phase=orphaned, current_node_id=NULL.
	rt, err := selectVMRuntime(t, ctx, h.store, vmID)
	if err != nil {
		t.Fatalf("select vm_runtime: %v", err)
	}
	if rt.Phase != store.VmPhaseOrphaned {
		t.Errorf("phase = %q, want orphaned", rt.Phase)
	}
	if rt.CurrentNodeID != nil {
		t.Errorf("current_node_id = %v, want nil", rt.CurrentNodeID)
	}

	// vms.desired_phase is NOT touched per migration 00003.
	desired := selectVMDesiredPhase(t, ctx, h.store, vmID)
	if desired != store.VmDesiredPhaseRunning {
		t.Errorf("desired_phase = %q, want running (must NOT change on force-delete)", desired)
	}
}

func TestE2E_Nodes_ForceDeleteCancelsActiveMigration(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	sourceIDStr := createNodeAsAdmin(t, h, uniqueName("mig-src"))
	targetIDStr := createNodeAsAdmin(t, h, uniqueName("mig-tgt"))
	sourceID := uuid.MustParse(lookupNodeID(t, h, sourceIDStr))
	targetID := uuid.MustParse(lookupNodeID(t, h, targetIDStr))

	ownerID, _, _ := seedUserWithRole(t, ctx, h.store, auth.RoleDeveloper)
	vmID := seedVMOnNode(t, ctx, h.store, ownerID, sourceID)
	migID := seedActiveMigration(t, ctx, h.store, vmID, sourceID, targetID)

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.delete(t, "/v1/nodes/"+sourceIDStr+"?force=true", adminToken)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("force-delete status = %d, want 204", resp.StatusCode)
	}

	phase, errMsg := selectMigrationPhase(t, ctx, h.store, migID)
	if phase != store.MigrationPhaseCancelled {
		t.Errorf("migration phase = %q, want cancelled", phase)
	}
	if errMsg == nil || *errMsg == "" {
		t.Errorf("migration error_message = %v, want non-empty", errMsg)
	}
}

// ---- helpers below: direct DB seeding for relations not exposed via the API yet ----

// seedVMOnNode inserts a vms row plus a vm_runtime row pinned to nodeID
// (phase=running). The schema still demands a NOT NULL machine_type and
// architecture; we use 'pc-i440fx-8.0' as a placeholder.
func seedVMOnNode(t *testing.T, ctx context.Context, s *store.Store, ownerID, nodeID uuid.UUID) uuid.UUID {
	t.Helper()
	vmID := uuid.New()
	pool := s.Pool()
	const insVM = `
		insert into vms
		  (id, owner_id, name, architecture, cpu_cores, memory_mib, machine_type)
		values
		  ($1, $2, $3, 'amd64', 1, 256, 'pc-i440fx-8.0')`
	if _, err := pool.Exec(ctx, insVM, vmID, ownerID, "vm-"+uuid.NewString()[:8]); err != nil {
		t.Fatalf("insert vms: %v", err)
	}
	const insRT = `
		insert into vm_runtime (vm_id, current_node_id, phase)
		values ($1, $2, 'running')`
	if _, err := pool.Exec(ctx, insRT, vmID, nodeID); err != nil {
		t.Fatalf("insert vm_runtime: %v", err)
	}
	return vmID
}

// seedActiveMigration inserts a migrations row in 'active' phase from
// source to target for vmID.
func seedActiveMigration(t *testing.T, ctx context.Context, s *store.Store, vmID, sourceID, targetID uuid.UUID) uuid.UUID {
	t.Helper()
	migID := uuid.New()
	const ins = `
		insert into migrations
		  (id, vm_id, source_node_id, target_node_id, reason, phase)
		values
		  ($1, $2, $3, $4, 'manual', 'active')`
	if _, err := s.Pool().Exec(ctx, ins, migID, vmID, sourceID, targetID); err != nil {
		t.Fatalf("insert migrations: %v", err)
	}
	return migID
}

// selectVMRuntime returns the runtime row for vmID. Used to assert
// post-force-delete state.
func selectVMRuntime(t *testing.T, ctx context.Context, s *store.Store, vmID uuid.UUID) (store.VMRuntime, error) {
	t.Helper()
	const q = `
		select vm_id, current_node_id, phase, conditions, observed_generation,
		       qemu_pid, qmp_socket_path, vnc_port, spice_port, serial_console_path,
		       guest_os, guest_hostname, guest_ip_addresses, cpu_usage_percent,
		       memory_used_mib, last_started_at, last_stopped_at, last_error_at,
		       last_error_message, last_observed_at, updated_at
		from vm_runtime where vm_id = $1`
	row := s.Pool().QueryRow(ctx, q, vmID)
	var rt store.VMRuntime
	err := row.Scan(
		&rt.VmID, &rt.CurrentNodeID, &rt.Phase, &rt.Conditions, &rt.ObservedGeneration,
		&rt.QEMUPID, &rt.QmpSocketPath, &rt.VncPort, &rt.SpicePort, &rt.SerialConsolePath,
		&rt.GuestOs, &rt.GuestHostname, &rt.GuestIpAddresses, &rt.CpuUsagePercent,
		&rt.MemoryUsedMib, &rt.LastStartedAt, &rt.LastStoppedAt, &rt.LastErrorAt,
		&rt.LastErrorMessage, &rt.LastObservedAt, &rt.UpdatedAt,
	)
	return rt, err
}

func selectVMDesiredPhase(t *testing.T, ctx context.Context, s *store.Store, vmID uuid.UUID) store.VMDesiredPhase {
	t.Helper()
	var phase store.VMDesiredPhase
	if err := s.Pool().QueryRow(ctx, `select desired_phase from vms where id = $1`, vmID).Scan(&phase); err != nil {
		t.Fatalf("select vms.desired_phase: %v", err)
	}
	return phase
}

func selectMigrationPhase(t *testing.T, ctx context.Context, s *store.Store, migID uuid.UUID) (store.MigrationPhase, *string) {
	t.Helper()
	var (
		phase  store.MigrationPhase
		errMsg *string
	)
	if err := s.Pool().QueryRow(ctx, `select phase, error_message from migrations where id = $1`, migID).Scan(&phase, &errMsg); err != nil {
		t.Fatalf("select migrations: %v", err)
	}
	return phase, errMsg
}
