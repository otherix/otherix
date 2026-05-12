// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api"
	storagepoolshandlers "github.com/otherix/otherix/internal/api/handlers/storagepools"
	"github.com/otherix/otherix/internal/api/middleware"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/store"
)

// taskViewE2E mirrors the OpenAPI Task schema for assertion ergonomics.
// Result/Error are RawMessage so absent JSONB columns surface as
// `null` (json.RawMessage("null") is the Go literal value when sqlc
// returned a nil byte slice; the handler emits `null` via nil
// RawMessage on the wire).
type taskViewE2E struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Status       string          `json:"status"`
	Progress     *int            `json:"progress"`
	ResourceType string          `json:"resource_type"`
	ResourceID   *string         `json:"resource_id"`
	Result       json.RawMessage `json:"result"`
	Error        json.RawMessage `json:"error"`
	Attempts     int             `json:"attempts"`
	MaxAttempts  int             `json:"max_attempts"`
	CreatedAt    string          `json:"created_at"`
	StartedAt    *string         `json:"started_at"`
	FinishedAt   *string         `json:"finished_at"`
}

type taskListE2E struct {
	Data []taskViewE2E `json:"data"`
	Meta struct {
		NextCursor *string `json:"next_cursor"`
	} `json:"meta"`
}

// seedTaskOpts captures the fields tests want to vary; everything else
// gets a sane default so call sites stay terse.
type seedTaskOpts struct {
	taskType     string
	resourceType string
	resourceID   *uuid.UUID
	createdBy    *uuid.UUID
}

// seedTaskE2E inserts a tasks row directly through the store. Used
// by integration tests that need to bypass the public task-creation
// endpoints and stage rows directly.
func seedTaskE2E(t *testing.T, ctx context.Context, s *store.Store, opts seedTaskOpts) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if opts.taskType == "" {
		opts.taskType = "storage_pool.scan"
	}
	if opts.resourceType == "" {
		opts.resourceType = "storage_pool"
	}
	if _, err := s.Queries().CreateTask(ctx, store.CreateTaskParams{
		ID:           id,
		Type:         opts.taskType,
		Status:       store.TaskStatusPending,
		ResourceType: opts.resourceType,
		ResourceID:   opts.resourceID,
		Args:         []byte(`{}`),
		MaxAttempts:  25,
		CreatedBy:    opts.createdBy,
	}); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return id
}

// idsOf collects task ids — used to assert membership without caring
// about ordering (cross-test pollution may pull in unrelated rows).
func idsOf(views []taskViewE2E) map[string]bool {
	out := make(map[string]bool, len(views))
	for _, v := range views {
		out[v.ID] = true
	}
	return out
}

// ---- list: RBAC scope ----

func TestE2E_Tasks_List_AdminSeesCrossUser(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	devID, _, _ := seedUserWithRole(t, ctx, h.store, auth.RoleDeveloper)
	otherID, _, _ := seedUserWithRole(t, ctx, h.store, auth.RoleDeveloper)

	devTask := seedTaskE2E(t, ctx, h.store, seedTaskOpts{createdBy: &devID})
	otherTask := seedTaskE2E(t, ctx, h.store, seedTaskOpts{createdBy: &otherID})

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.get(t, "/v1/tasks?limit=200", adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var list taskListE2E
	decodeJSON(t, resp, &list)
	got := idsOf(list.Data)
	if !got[devTask.String()] {
		t.Errorf("admin list missing dev task %v", devTask)
	}
	if !got[otherTask.String()] {
		t.Errorf("admin list missing other-user task %v", otherTask)
	}
}

func TestE2E_Tasks_List_DeveloperSeesOwnOnly(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()

	// Seed two users; only the caller's tasks should be visible.
	otherID, _, _ := seedUserWithRole(t, ctx, h.store, auth.RoleDeveloper)
	otherTask := seedTaskE2E(t, ctx, h.store, seedTaskOpts{createdBy: &otherID})

	devID, devEmail, devPW := seedUserWithRole(t, ctx, h.store, auth.RoleDeveloper)
	loginResp := h.post(t, "/v1/auth/login",
		map[string]string{"email": devEmail, "password": devPW}, "")
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("login: status = %d, want 200", loginResp.StatusCode)
	}
	var login struct {
		AccessToken string `json:"access_token"`
	}
	decodeJSON(t, loginResp, &login)

	devTask := seedTaskE2E(t, ctx, h.store, seedTaskOpts{createdBy: &devID})

	resp := h.get(t, "/v1/tasks?limit=200", login.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var list taskListE2E
	decodeJSON(t, resp, &list)
	got := idsOf(list.Data)
	if !got[devTask.String()] {
		t.Errorf("developer list missing own task %v", devTask)
	}
	if got[otherTask.String()] {
		t.Errorf("developer list leaked other-user task %v", otherTask)
	}
}

func TestE2E_Tasks_List_ViewerSeesOwnOnly(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()

	otherID, _, _ := seedUserWithRole(t, ctx, h.store, auth.RoleDeveloper)
	otherTask := seedTaskE2E(t, ctx, h.store, seedTaskOpts{createdBy: &otherID})

	viewerID, viewerEmail, viewerPW := seedUserWithRole(t, ctx, h.store, auth.RoleViewer)
	loginResp := h.post(t, "/v1/auth/login",
		map[string]string{"email": viewerEmail, "password": viewerPW}, "")
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("login: status = %d, want 200", loginResp.StatusCode)
	}
	var login struct {
		AccessToken string `json:"access_token"`
	}
	decodeJSON(t, loginResp, &login)

	viewerTask := seedTaskE2E(t, ctx, h.store, seedTaskOpts{createdBy: &viewerID})

	resp := h.get(t, "/v1/tasks?limit=200", login.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var list taskListE2E
	decodeJSON(t, resp, &list)
	got := idsOf(list.Data)
	if !got[viewerTask.String()] {
		t.Errorf("viewer list missing own task %v", viewerTask)
	}
	if got[otherTask.String()] {
		t.Errorf("viewer list leaked other-user task %v", otherTask)
	}
}

// ---- list: filters ----

func TestE2E_Tasks_List_FilterByStatus(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	devID, _, _ := seedUserWithRole(t, ctx, h.store, auth.RoleDeveloper)

	pendingTask := seedTaskE2E(t, ctx, h.store, seedTaskOpts{createdBy: &devID})
	runningTask := seedTaskE2E(t, ctx, h.store, seedTaskOpts{createdBy: &devID})
	if err := h.store.Queries().UpdateTaskRunning(ctx, runningTask); err != nil {
		t.Fatalf("UpdateTaskRunning: %v", err)
	}

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.get(t, "/v1/tasks?status=running&limit=200", adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var list taskListE2E
	decodeJSON(t, resp, &list)

	got := idsOf(list.Data)
	if !got[runningTask.String()] {
		t.Errorf("status=running missing %v", runningTask)
	}
	if got[pendingTask.String()] {
		t.Errorf("status=running leaked pending task %v", pendingTask)
	}
	for _, v := range list.Data {
		if v.Status != "running" {
			t.Errorf("returned task %v with status=%q under status=running filter", v.ID, v.Status)
		}
	}
}

func TestE2E_Tasks_List_FilterByType(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	devID, _, _ := seedUserWithRole(t, ctx, h.store, auth.RoleDeveloper)

	scanTask := seedTaskE2E(t, ctx, h.store, seedTaskOpts{
		taskType: "storage_pool.scan", createdBy: &devID,
	})
	importTask := seedTaskE2E(t, ctx, h.store, seedTaskOpts{
		taskType: "template.import", resourceType: "template", createdBy: &devID,
	})

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.get(t, "/v1/tasks?type=template.import&limit=200", adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var list taskListE2E
	decodeJSON(t, resp, &list)
	got := idsOf(list.Data)
	if !got[importTask.String()] {
		t.Errorf("type=template.import missing %v", importTask)
	}
	if got[scanTask.String()] {
		t.Errorf("type=template.import leaked %v", scanTask)
	}
}

func TestE2E_Tasks_List_FilterByResource(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	devID, _, _ := seedUserWithRole(t, ctx, h.store, auth.RoleDeveloper)

	resA := uuid.New()
	resB := uuid.New()
	taskA := seedTaskE2E(t, ctx, h.store, seedTaskOpts{
		resourceID: &resA, createdBy: &devID,
	})
	taskB := seedTaskE2E(t, ctx, h.store, seedTaskOpts{
		resourceID: &resB, createdBy: &devID,
	})

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.get(t,
		"/v1/tasks?resource_type=storage_pool&resource_id="+resA.String()+"&limit=200",
		adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var list taskListE2E
	decodeJSON(t, resp, &list)
	got := idsOf(list.Data)
	if !got[taskA.String()] {
		t.Errorf("resource filter missing %v", taskA)
	}
	if got[taskB.String()] {
		t.Errorf("resource filter leaked %v", taskB)
	}
}

// ---- list: pagination ----

func TestE2E_Tasks_List_CursorPagination(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	devID, _, _ := seedUserWithRole(t, ctx, h.store, auth.RoleDeveloper)

	const total = 5
	resID := uuid.New()
	want := make(map[string]bool, total)
	for i := 0; i < total; i++ {
		id := seedTaskE2E(t, ctx, h.store, seedTaskOpts{
			resourceID: &resID, createdBy: &devID,
		})
		want[id.String()] = true
		// Spread CreatedAt so cursor pagination has a deterministic
		// ordering keyed on the timestamp axis.
		time.Sleep(2 * time.Millisecond)
	}

	adminToken := loginAs(t, h, auth.RoleAdmin)
	q := "/v1/tasks?resource_id=" + resID.String() + "&limit=2"
	resp := h.get(t, q, adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("page1 status = %d, want 200", resp.StatusCode)
	}
	var page1 taskListE2E
	decodeJSON(t, resp, &page1)
	if len(page1.Data) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(page1.Data))
	}
	if page1.Meta.NextCursor == nil || *page1.Meta.NextCursor == "" {
		t.Fatal("page1 next_cursor is nil/empty, want next page")
	}

	resp = h.get(t, q+"&cursor="+*page1.Meta.NextCursor, adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("page2 status = %d, want 200", resp.StatusCode)
	}
	var page2 taskListE2E
	decodeJSON(t, resp, &page2)

	// Walk pages until we collect all five — page2 may carry the
	// terminal cursor or another non-final cursor depending on the
	// limit divisibility. Two pages of two leaves one row, so a
	// third hop is required.
	collected := append([]taskViewE2E{}, page1.Data...)
	collected = append(collected, page2.Data...)
	if page2.Meta.NextCursor != nil && *page2.Meta.NextCursor != "" {
		resp = h.get(t, q+"&cursor="+*page2.Meta.NextCursor, adminToken)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("page3 status = %d, want 200", resp.StatusCode)
		}
		var page3 taskListE2E
		decodeJSON(t, resp, &page3)
		collected = append(collected, page3.Data...)
		if page3.Meta.NextCursor != nil && *page3.Meta.NextCursor != "" {
			t.Errorf("page3 next_cursor = %v, want nil for terminal page", *page3.Meta.NextCursor)
		}
	}

	got := idsOf(collected)
	for w := range want {
		if !got[w] {
			t.Errorf("paginated walk missing %v", w)
		}
	}
	if len(collected) != total {
		t.Errorf("paginated len = %d, want %d", len(collected), total)
	}
}

// ---- list: validation ----

func TestE2E_Tasks_List_BadCursor(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.get(t, "/v1/tasks?cursor=not-base64!", adminToken)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	if b.Error.Code != response.CodeValidationFailed {
		t.Errorf("code = %q, want %q", b.Error.Code, response.CodeValidationFailed)
	}
}

func TestE2E_Tasks_List_BadStatusFilter(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.get(t, "/v1/tasks?status=bogus", adminToken)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	if b.Error.Code != response.CodeValidationFailed {
		t.Errorf("code = %q, want %q", b.Error.Code, response.CodeValidationFailed)
	}
}

func TestE2E_Tasks_List_BadResourceID(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.get(t, "/v1/tasks?resource_id=not-a-uuid", adminToken)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	if b.Error.Code != response.CodeValidationFailed {
		t.Errorf("code = %q, want %q", b.Error.Code, response.CodeValidationFailed)
	}
}

// ---- get ----

func TestE2E_Tasks_Get_OwnHappy(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	devID, devEmail, devPW := seedUserWithRole(t, ctx, h.store, auth.RoleDeveloper)
	resID := uuid.New()
	taskID := seedTaskE2E(t, ctx, h.store, seedTaskOpts{
		resourceID: &resID, createdBy: &devID,
	})

	loginResp := h.post(t, "/v1/auth/login",
		map[string]string{"email": devEmail, "password": devPW}, "")
	var login struct {
		AccessToken string `json:"access_token"`
	}
	decodeJSON(t, loginResp, &login)

	resp := h.get(t, "/v1/tasks/"+taskID.String(), login.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got taskViewE2E
	decodeJSON(t, resp, &got)
	if got.ID != taskID.String() {
		t.Errorf("ID = %q, want %q", got.ID, taskID)
	}
	if got.Status != "pending" {
		t.Errorf("Status = %q, want pending", got.Status)
	}
	if got.Type != "storage_pool.scan" {
		t.Errorf("Type = %q, want storage_pool.scan", got.Type)
	}
	if got.ResourceID == nil || *got.ResourceID != resID.String() {
		t.Errorf("ResourceID = %v, want %v", got.ResourceID, resID)
	}
	// Internal columns must NOT leak into the JSON body. Decode raw
	// to assert the keys are absent.
	rawResp := h.get(t, "/v1/tasks/"+taskID.String(), login.AccessToken)
	var raw map[string]json.RawMessage
	decodeJSON(t, rawResp, &raw)
	for _, internal := range []string{"args", "river_job_id", "created_by"} {
		if _, leaked := raw[internal]; leaked {
			t.Errorf("internal field %q leaked into Task view", internal)
		}
	}
	// Result / Error nullable fields surface as JSON null when absent.
	for _, k := range []string{"result", "error"} {
		v, ok := raw[k]
		if !ok {
			t.Errorf("Task view missing %q key", k)
			continue
		}
		if string(v) != "null" {
			t.Errorf("%q on a fresh task = %s, want null", k, string(v))
		}
	}
}

func TestE2E_Tasks_Get_AdminCrossUser(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	otherID, _, _ := seedUserWithRole(t, ctx, h.store, auth.RoleDeveloper)
	taskID := seedTaskE2E(t, ctx, h.store, seedTaskOpts{createdBy: &otherID})

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.get(t, "/v1/tasks/"+taskID.String(), adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (admin cross-user)", resp.StatusCode)
	}
}

func TestE2E_Tasks_Get_DeveloperCrossUserReturns404(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	otherID, _, _ := seedUserWithRole(t, ctx, h.store, auth.RoleDeveloper)
	taskID := seedTaskE2E(t, ctx, h.store, seedTaskOpts{createdBy: &otherID})

	_, devEmail, devPW := seedUserWithRole(t, ctx, h.store, auth.RoleDeveloper)
	loginResp := h.post(t, "/v1/auth/login",
		map[string]string{"email": devEmail, "password": devPW}, "")
	var login struct {
		AccessToken string `json:"access_token"`
	}
	decodeJSON(t, loginResp, &login)

	resp := h.get(t, "/v1/tasks/"+taskID.String(), login.AccessToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no existence leak)", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	if b.Error.Code != response.CodeNotFound {
		t.Errorf("code = %q, want %q", b.Error.Code, response.CodeNotFound)
	}
}

func TestE2E_Tasks_Get_Missing(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.get(t, "/v1/tasks/"+uuid.New().String(), adminToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	if b.Error.Code != response.CodeNotFound {
		t.Errorf("code = %q, want %q", b.Error.Code, response.CodeNotFound)
	}
}

func TestE2E_Tasks_Get_BadID(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.get(t, "/v1/tasks/not-a-uuid", adminToken)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	if b.Error.Code != response.CodeValidationFailed {
		t.Errorf("code = %q, want %q", b.Error.Code, response.CodeValidationFailed)
	}
}

// ---- cancel ----

func TestE2E_Tasks_Cancel_PendingHappy(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	devID, _, _ := seedUserWithRole(t, ctx, h.store, auth.RoleDeveloper)
	taskID := seedTaskE2E(t, ctx, h.store, seedTaskOpts{createdBy: &devID})

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.post(t, "/v1/tasks/"+taskID.String()+"/cancel", map[string]any{}, adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got taskViewE2E
	decodeJSON(t, resp, &got)
	if got.Status != "cancelled" {
		t.Errorf("Status = %q, want cancelled", got.Status)
	}
	if got.FinishedAt == nil {
		t.Error("FinishedAt = nil, want set")
	}

	row, err := h.store.Queries().GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if row.Status != store.TaskStatusCancelled {
		t.Errorf("DB.Status = %q, want cancelled", row.Status)
	}
}

func TestE2E_Tasks_Cancel_OwnHappy(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	devID, devEmail, devPW := seedUserWithRole(t, ctx, h.store, auth.RoleDeveloper)
	taskID := seedTaskE2E(t, ctx, h.store, seedTaskOpts{createdBy: &devID})

	loginResp := h.post(t, "/v1/auth/login",
		map[string]string{"email": devEmail, "password": devPW}, "")
	var login struct {
		AccessToken string `json:"access_token"`
	}
	decodeJSON(t, loginResp, &login)

	resp := h.post(t, "/v1/tasks/"+taskID.String()+"/cancel", map[string]any{}, login.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestE2E_Tasks_Cancel_Running409(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	devID, _, _ := seedUserWithRole(t, ctx, h.store, auth.RoleDeveloper)
	taskID := seedTaskE2E(t, ctx, h.store, seedTaskOpts{createdBy: &devID})
	if err := h.store.Queries().UpdateTaskRunning(ctx, taskID); err != nil {
		t.Fatalf("UpdateTaskRunning: %v", err)
	}

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.post(t, "/v1/tasks/"+taskID.String()+"/cancel", map[string]any{}, adminToken)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	if b.Error.Code != response.CodeConflict {
		t.Errorf("error.code = %q, want %q", b.Error.Code, response.CodeConflict)
	}
	if got, _ := b.Error.Details["code"].(string); got != "task_not_cancellable" {
		t.Errorf("details.code = %v, want task_not_cancellable", b.Error.Details["code"])
	}

	// State unchanged.
	row, err := h.store.Queries().GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if row.Status != store.TaskStatusRunning {
		t.Errorf("DB.Status = %q, want running (unchanged)", row.Status)
	}
}

func TestE2E_Tasks_Cancel_Terminal409(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	devID, _, _ := seedUserWithRole(t, ctx, h.store, auth.RoleDeveloper)

	for _, terminal := range []store.TaskStatus{
		store.TaskStatusSuccess,
		store.TaskStatusFailed,
		store.TaskStatusCancelled,
	} {
		t.Run(string(terminal), func(t *testing.T) {
			taskID := seedTaskE2E(t, ctx, h.store, seedTaskOpts{createdBy: &devID})
			if err := h.store.Queries().UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{
				ID:     taskID,
				Status: terminal,
			}); err != nil {
				t.Fatalf("UpdateTaskFinalized(%v): %v", terminal, err)
			}

			adminToken := loginAs(t, h, auth.RoleAdmin)
			resp := h.post(t, "/v1/tasks/"+taskID.String()+"/cancel", map[string]any{}, adminToken)
			if resp.StatusCode != http.StatusConflict {
				t.Fatalf("status = %d, want 409", resp.StatusCode)
			}
			var b response.ErrorBody
			decodeJSON(t, resp, &b)
			if got, _ := b.Error.Details["code"].(string); got != "task_already_finalized" {
				t.Errorf("details.code = %v, want task_already_finalized", b.Error.Details["code"])
			}
		})
	}
}

func TestE2E_Tasks_Cancel_Missing404(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.post(t, "/v1/tasks/"+uuid.New().String()+"/cancel", map[string]any{}, adminToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	if b.Error.Code != response.CodeNotFound {
		t.Errorf("error.code = %q, want %q", b.Error.Code, response.CodeNotFound)
	}
}

func TestE2E_Tasks_Cancel_BadID400(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.post(t, "/v1/tasks/not-a-uuid/cancel", map[string]any{}, adminToken)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestE2E_Tasks_Cancel_DeveloperCrossUser404(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	otherID, _, _ := seedUserWithRole(t, ctx, h.store, auth.RoleDeveloper)
	taskID := seedTaskE2E(t, ctx, h.store, seedTaskOpts{createdBy: &otherID})

	_, devEmail, devPW := seedUserWithRole(t, ctx, h.store, auth.RoleDeveloper)
	loginResp := h.post(t, "/v1/auth/login",
		map[string]string{"email": devEmail, "password": devPW}, "")
	var login struct {
		AccessToken string `json:"access_token"`
	}
	decodeJSON(t, loginResp, &login)

	resp := h.post(t, "/v1/tasks/"+taskID.String()+"/cancel", map[string]any{}, login.AccessToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no existence leak)", resp.StatusCode)
	}
}

func TestE2E_Tasks_Cancel_ViewerForbidden403(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	viewerID, viewerEmail, viewerPW := seedUserWithRole(t, ctx, h.store, auth.RoleViewer)
	taskID := seedTaskE2E(t, ctx, h.store, seedTaskOpts{createdBy: &viewerID})

	loginResp := h.post(t, "/v1/auth/login",
		map[string]string{"email": viewerEmail, "password": viewerPW}, "")
	var login struct {
		AccessToken string `json:"access_token"`
	}
	decodeJSON(t, loginResp, &login)

	resp := h.post(t, "/v1/tasks/"+taskID.String()+"/cancel", map[string]any{}, login.AccessToken)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (viewer lacks task:cancel)", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	if b.Error.Code != response.CodePermissionDenied {
		t.Errorf("error.code = %q, want %q", b.Error.Code, response.CodePermissionDenied)
	}
}

func TestE2E_Tasks_Cancel_PendingWithRealRiverJob(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	devID, _, _ := seedUserWithRole(t, ctx, h.store, auth.RoleDeveloper)
	taskID := seedTaskE2E(t, ctx, h.store, seedTaskOpts{createdBy: &devID})

	// Insert a real river_job (without Start, so the worker never picks
	// it up) and stamp its id on the task. The cancel handler exercises
	// its JobCancelTx branch this way.
	riverClient, err := api.BuildRiverClient(api.RiverDeps{
		Pool:   sharedHarness.Pool,
		Cfg:    config.WorkersConfig{Enabled: false, MaxWorkers: 1},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:  h.store,
	})
	if err != nil {
		t.Fatalf("BuildRiverClient: %v", err)
	}
	insertResult, err := riverClient.Insert(ctx, storagepoolshandlers.StoragePoolScanArgs{
		TaskID: taskID,
		PoolID: uuid.New(),
	}, nil)
	if err != nil {
		t.Fatalf("riverClient.Insert: %v", err)
	}
	jobID := insertResult.Job.ID
	if err := h.store.Queries().UpdateTaskRiverJobID(ctx, store.UpdateTaskRiverJobIDParams{
		ID:         taskID,
		RiverJobID: &jobID,
	}); err != nil {
		t.Fatalf("UpdateTaskRiverJobID: %v", err)
	}

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.post(t, "/v1/tasks/"+taskID.String()+"/cancel", map[string]any{}, adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Task is cancelled.
	row, err := h.store.Queries().GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if row.Status != store.TaskStatusCancelled {
		t.Errorf("task.Status = %q, want cancelled", row.Status)
	}

	// river_job state is also cancelled (transactional handoff worked).
	var jobState string
	if err := sharedHarness.Pool.QueryRow(ctx,
		`select state::text from river_job where id = $1`, jobID,
	).Scan(&jobState); err != nil {
		t.Fatalf("query river_job: %v", err)
	}
	if jobState != "cancelled" {
		t.Errorf("river_job.state = %q, want cancelled", jobState)
	}
}

func TestE2E_Tasks_Cancel_IdempotencyReplay(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	devID, _, _ := seedUserWithRole(t, ctx, h.store, auth.RoleDeveloper)
	taskID := seedTaskE2E(t, ctx, h.store, seedTaskOpts{createdBy: &devID})

	adminToken := loginAs(t, h, auth.RoleAdmin)
	idemKey := "cancel-idem-" + uuid.NewString()

	// First request.
	first := h.postWithHeaders(t, "/v1/tasks/"+taskID.String()+"/cancel",
		map[string]any{}, adminToken,
		map[string]string{middleware.HeaderIdempotencyKey: idemKey})
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d, want 200", first.StatusCode)
	}
	firstBody, err := io.ReadAll(first.Body)
	if err != nil {
		t.Fatalf("read first body: %v", err)
	}
	first.Body.Close()

	// Second request with same key + body — middleware replays cached
	// 200 verbatim. Middleware does not re-enter the handler,
	// so no actual re-cancellation logic runs.
	second := h.postWithHeaders(t, "/v1/tasks/"+taskID.String()+"/cancel",
		map[string]any{}, adminToken,
		map[string]string{middleware.HeaderIdempotencyKey: idemKey})
	if second.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d, want 200 (cached replay)", second.StatusCode)
	}
	secondBody, err := io.ReadAll(second.Body)
	if err != nil {
		t.Fatalf("read second body: %v", err)
	}
	second.Body.Close()
	if !bytes.Equal(firstBody, secondBody) {
		t.Errorf("replayed body differs from first:\n  first:  %s\n  second: %s",
			firstBody, secondBody)
	}
}
