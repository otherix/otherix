// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package migrations_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/handlers/migrations"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// newHandler builds a migrations.Handler over the real etcdstore.
func newHandler(s *etcdstore.Store) *migrations.Handler {
	return migrations.New(s, discardLogger())
}

// ownedVM seeds a pinned VM owned by owner so ownership checks resolve.
func ownedVM(t *testing.T, cli *etcd.Client, node, owner uuid.UUID) store.VM {
	t.Helper()
	pin := node
	v := store.VM{
		ID: uuid.New(), OwnerID: owner, Name: "mig-" + uuid.NewString()[:8],
		DesiredPhase: store.VmDesiredPhaseRunning, Architecture: store.CpuArchAmd64,
		CpuCores: 2, MemoryMib: 2048, PinnedNodeID: &pin,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	ctx := context.Background()
	if err := cli.PutJSON(ctx, etcd.Key("vms", v.ID.String()), v); err != nil {
		t.Fatalf("seed vm: %v", err)
	}
	if err := cli.Put(ctx, etcd.Key("uniq", "vms", "name", v.Name), []byte(v.ID.String())); err != nil {
		t.Fatalf("seed vm guard: %v", err)
	}
	if err := cli.Put(ctx, etcd.Key("index", "vms", "pinned_node", node.String(), v.ID.String()), []byte(v.ID.String())); err != nil {
		t.Fatalf("seed pinned_node index: %v", err)
	}
	return v
}

// migrateRequest builds a POST /v1/vms/{id}/migrate request carrying the caller
// principal in context and the chi route param `id` = vmName.
func migrateRequest(caller *auth.User, vmName string, body any) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/vms/"+vmName+"/migrate", &buf)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", vmName)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = auth.WithUser(ctx, caller)
	return req.WithContext(ctx)
}

func operator(id uuid.UUID) *auth.User {
	return &auth.User{ID: id, Role: auth.RoleOperator, Type: auth.TypeJWT}
}

func developer(id uuid.UUID) *auth.User {
	return &auth.User{ID: id, Role: auth.RoleDeveloper, Type: auth.TypeJWT}
}

// --- T13: POST /v1/vms/{id}/migrate ---

func TestMigrate_Accepted(t *testing.T) {
	s, cli := freshStore(t)
	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	owner := uuid.New()
	vm := ownedVM(t, cli, nodeA.ID, owner)

	h := newHandler(s)
	rec := httptest.NewRecorder()
	h.Start(rec, migrateRequest(operator(owner), vm.Name, map[string]any{}))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body=%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		TaskID string `json:"task_id"`
		Status string `json:"status"`
		Links  struct {
			Self string `json:"self"`
		} `json:"links"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "pending" {
		t.Errorf("status = %q, want pending", body.Status)
	}
	if _, err := uuid.Parse(body.TaskID); err != nil {
		t.Errorf("task_id = %q, not a uuid", body.TaskID)
	}
	if body.Links.Self != "/v1/tasks/"+body.TaskID {
		t.Errorf("links.self = %q, want /v1/tasks/%s", body.Links.Self, body.TaskID)
	}

	// A migration row now exists for the VM.
	ms, err := s.ListMigrations(context.Background(), store.ListMigrationsParams{LimitCount: 10, VmID: &vm.ID})
	if err != nil {
		t.Fatalf("ListMigrations: %v", err)
	}
	if len(ms) != 1 {
		t.Fatalf("migrations for vm = %d, want 1", len(ms))
	}
	if ms[0].SourceNodeID == nil || *ms[0].SourceNodeID != nodeA.ID {
		t.Errorf("source = %v, want %v", ms[0].SourceNodeID, nodeA.ID)
	}
	if ms[0].InitiatedByUserID == nil || *ms[0].InitiatedByUserID != owner {
		t.Errorf("initiated_by = %v, want %v", ms[0].InitiatedByUserID, owner)
	}
}

func TestMigrate_ConflictWhenActiveExists(t *testing.T) {
	s, cli := freshStore(t)
	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	owner := uuid.New()
	vm := ownedVM(t, cli, nodeA.ID, owner)

	h := newHandler(s)
	first := httptest.NewRecorder()
	h.Start(first, migrateRequest(operator(owner), vm.Name, map[string]any{}))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202", first.Code)
	}

	second := httptest.NewRecorder()
	h.Start(second, migrateRequest(operator(owner), vm.Name, map[string]any{}))
	if second.Code != http.StatusConflict {
		t.Fatalf("second status = %d, want 409 (body=%s)", second.Code, second.Body.String())
	}
}

func TestMigrate_NoOpWhenTargetIsCurrent(t *testing.T) {
	s, cli := freshStore(t)
	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	owner := uuid.New()
	vm := ownedVM(t, cli, nodeA.ID, owner)

	h := newHandler(s)
	rec := httptest.NewRecorder()
	h.Start(rec, migrateRequest(operator(owner), vm.Name, map[string]any{"target_node": nodeA.Name}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Status        string `json:"status"`
		CurrentNodeID string `json:"current_node_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "already_on_target" {
		t.Errorf("status = %q, want already_on_target", body.Status)
	}
	if body.CurrentNodeID != nodeA.ID.String() {
		t.Errorf("current_node_id = %q, want %s", body.CurrentNodeID, nodeA.ID)
	}
	// No migration created.
	ms, _ := s.ListMigrations(context.Background(), store.ListMigrationsParams{LimitCount: 10, VmID: &vm.ID})
	if len(ms) != 0 {
		t.Errorf("migrations for vm = %d, want 0 (no-op)", len(ms))
	}
}

func TestMigrate_CrossUser404(t *testing.T) {
	s, cli := freshStore(t)
	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	owner := uuid.New()
	vm := ownedVM(t, cli, nodeA.ID, owner)

	h := newHandler(s)
	rec := httptest.NewRecorder()
	// A developer who is NOT the owner: scope=own on vm:migrate -> 404 no-leak.
	h.Start(rec, migrateRequest(developer(uuid.New()), vm.Name, map[string]any{}))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
}

// --- T14: GET /v1/migrations/{id} + list ---

// getRequest builds a GET /v1/migrations/{id} request with the caller and route param.
func getRequest(caller *auth.User, id string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/v1/migrations/"+id, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = auth.WithUser(ctx, caller)
	return req.WithContext(ctx)
}

func TestMigrationGet_View(t *testing.T) {
	s, cli := freshStore(t)
	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	owner := uuid.New()
	vm := ownedVM(t, cli, nodeA.ID, owner)
	m, _ := seedNodelessMigration(t, s, vm.ID, nodeA.ID)

	h := newHandler(s)
	rec := httptest.NewRecorder()
	h.Get(rec, getRequest(operator(owner), m.ID.String()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		ID           string `json:"id"`
		VMID         string `json:"vm_id"`
		SourceNodeID string `json:"source_node_id"`
		Phase        string `json:"phase"`
		Live         bool   `json:"live"`
		Stalled      bool   `json:"stalled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.ID != m.ID.String() {
		t.Errorf("id = %q, want %s", body.ID, m.ID)
	}
	if body.VMID != vm.ID.String() {
		t.Errorf("vm_id = %q, want %s", body.VMID, vm.ID)
	}
	if body.SourceNodeID != nodeA.ID.String() {
		t.Errorf("source_node_id = %q, want %s", body.SourceNodeID, nodeA.ID)
	}
	if body.Phase != string(store.MigrationPhasePending) {
		t.Errorf("phase = %q, want pending", body.Phase)
	}
	if !body.Live {
		t.Errorf("live = false, want true")
	}
	if body.Stalled {
		t.Errorf("stalled = true, want false (slice 1 always false)")
	}
}

func TestMigrationGet_Unknown404(t *testing.T) {
	s, _ := freshStore(t)
	h := newHandler(s)
	rec := httptest.NewRecorder()
	h.Get(rec, getRequest(operator(uuid.New()), uuid.NewString()))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestMigrationGet_BadUUID(t *testing.T) {
	s, _ := freshStore(t)
	h := newHandler(s)
	rec := httptest.NewRecorder()
	h.Get(rec, getRequest(operator(uuid.New()), "not-a-uuid"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

// listRequest builds a GET /v1/migrations request with optional raw query.
func listRequest(caller *auth.User, rawQuery string) *http.Request {
	target := "/v1/migrations"
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	ctx := auth.WithUser(req.Context(), caller)
	return req.WithContext(ctx)
}

func TestMigrationList_FilterByVM(t *testing.T) {
	s, cli := freshStore(t)
	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	owner := uuid.New()
	vm1 := ownedVM(t, cli, nodeA.ID, owner)
	vm2 := ownedVM(t, cli, nodeA.ID, owner)
	m1, _ := seedNodelessMigration(t, s, vm1.ID, nodeA.ID)
	_, _ = seedNodelessMigration(t, s, vm2.ID, nodeA.ID)

	h := newHandler(s)
	rec := httptest.NewRecorder()
	h.List(rec, listRequest(operator(owner), "vm="+vm1.ID.String()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []struct {
			ID   string `json:"id"`
			VMID string `json:"vm_id"`
		} `json:"data"`
		Meta struct {
			NextCursor *string `json:"next_cursor"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Data) != 1 {
		t.Fatalf("data len = %d, want 1 (filtered by vm)", len(body.Data))
	}
	if body.Data[0].ID != m1.ID.String() {
		t.Errorf("id = %q, want %s", body.Data[0].ID, m1.ID)
	}
	if body.Data[0].VMID != vm1.ID.String() {
		t.Errorf("vm_id = %q, want %s", body.Data[0].VMID, vm1.ID)
	}
}

func TestMigrationList_Paginates(t *testing.T) {
	s, cli := freshStore(t)
	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	owner := uuid.New()
	// Three migrations on three distinct VMs (one active migration per VM).
	for i := 0; i < 3; i++ {
		vm := ownedVM(t, cli, nodeA.ID, owner)
		seedNodelessMigration(t, s, vm.ID, nodeA.ID)
	}

	h := newHandler(s)
	rec := httptest.NewRecorder()
	h.List(rec, listRequest(operator(owner), "limit=2"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var page1 struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Meta struct {
			NextCursor *string `json:"next_cursor"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page1); err != nil {
		t.Fatalf("decode page1: %v", err)
	}
	if len(page1.Data) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(page1.Data))
	}
	if page1.Meta.NextCursor == nil {
		t.Fatalf("next_cursor = nil, want a cursor (more pages)")
	}

	rec2 := httptest.NewRecorder()
	h.List(rec2, listRequest(operator(owner), "limit=2&cursor="+*page1.Meta.NextCursor))
	var page2 struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Meta struct {
			NextCursor *string `json:"next_cursor"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &page2); err != nil {
		t.Fatalf("decode page2: %v", err)
	}
	if len(page2.Data) != 1 {
		t.Fatalf("page2 len = %d, want 1", len(page2.Data))
	}
	if page2.Meta.NextCursor != nil {
		t.Errorf("page2 next_cursor = %q, want nil (last page)", *page2.Meta.NextCursor)
	}
}

// --- T15: POST /v1/migrations/{id}/cancel ---

func cancelRequest(caller *auth.User, id string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/migrations/"+id+"/cancel", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = auth.WithUser(ctx, caller)
	return req.WithContext(ctx)
}

func TestMigrationCancel_Active200(t *testing.T) {
	s, cli := freshStore(t)
	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	owner := uuid.New()
	vm := ownedVM(t, cli, nodeA.ID, owner)
	m, _ := seedNodelessMigration(t, s, vm.ID, nodeA.ID)

	h := newHandler(s)
	rec := httptest.NewRecorder()
	h.Cancel(rec, cancelRequest(operator(owner), m.ID.String()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Phase string `json:"phase"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Phase != string(store.MigrationPhaseCancelled) {
		t.Errorf("phase = %q, want cancelled", body.Phase)
	}
}

func TestMigrationCancel_TerminalUnchanged200(t *testing.T) {
	s, cli := freshStore(t)
	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	owner := uuid.New()
	vm := ownedVM(t, cli, nodeA.ID, owner)
	m, _ := seedNodelessMigration(t, s, vm.ID, nodeA.ID)

	ctx := context.Background()
	// Drive it to a terminal phase first (cancel once).
	if _, err := s.CancelMigration(ctx, m.ID, "first cancel"); err != nil {
		t.Fatalf("pre-cancel: %v", err)
	}

	h := newHandler(s)
	rec := httptest.NewRecorder()
	h.Cancel(rec, cancelRequest(operator(owner), m.ID.String()))

	// Best-effort: already-terminal returns the current row at 200, unchanged.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Phase string `json:"phase"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Phase != string(store.MigrationPhaseCancelled) {
		t.Errorf("phase = %q, want cancelled (unchanged)", body.Phase)
	}
}

func TestMigrationCancel_Unknown404(t *testing.T) {
	s, _ := freshStore(t)
	h := newHandler(s)
	rec := httptest.NewRecorder()
	h.Cancel(rec, cancelRequest(operator(uuid.New()), uuid.NewString()))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
}
