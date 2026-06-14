// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cpclient_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// TestStartMigration_Accepted covers the 202 path: the body is POSTed to
// /v1/vms/{id}/migrate and an AsyncTaskAccepted is returned with NoOp=false.
func TestStartMigration_Accepted(t *testing.T) {
	t.Parallel()
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/vms/web-1/migrate" {
			t.Errorf("path = %s, want /v1/vms/web-1/migrate", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"task_id": "11111111-1111-1111-1111-111111111111",
			"status":  "pending",
			"links":   map[string]any{"self": "/v1/tasks/11111111-1111-1111-1111-111111111111"},
		})
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	node := "node-b"
	bw := int64(100_000_000)
	res, err := c.StartMigration(context.Background(), "web-1", cpclient.MigrateBody{
		TargetNode:        &node,
		Live:              true,
		MaxBandwidthBytes: &bw,
	})
	if err != nil {
		t.Fatalf("StartMigration: %v", err)
	}
	if res.NoOp {
		t.Errorf("NoOp = true, want false for a 202 task")
	}
	if res.Task.TaskID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("task id = %q, want the 202 task id", res.Task.TaskID)
	}
	if gotBody["target_node"] != "node-b" {
		t.Errorf("body target_node = %v, want node-b", gotBody["target_node"])
	}
	if gotBody["live"] != true {
		t.Errorf("body live = %v, want true", gotBody["live"])
	}
	if gotBody["max_bandwidth_bytes"] != float64(100_000_000) {
		t.Errorf("body max_bandwidth_bytes = %v, want 100000000", gotBody["max_bandwidth_bytes"])
	}
	// target_pool / max_downtime_ms / allow_postcopy were unset -> omitted.
	if _, ok := gotBody["target_pool"]; ok {
		t.Errorf("body carried target_pool, want omitted")
	}
}

// TestStartMigration_NoOp covers the 200 already_on_target short-circuit: the
// result is flagged NoOp with the current node id and no task is present.
func TestStartMigration_NoOp(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":          "already_on_target",
			"current_node_id": "22222222-2222-2222-2222-222222222222",
		})
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	node := "node-a"
	res, err := c.StartMigration(context.Background(), "web-1", cpclient.MigrateBody{TargetNode: &node, Live: true})
	if err != nil {
		t.Fatalf("StartMigration: %v", err)
	}
	if !res.NoOp {
		t.Fatalf("NoOp = false, want true for already_on_target")
	}
	if res.CurrentNodeID != "22222222-2222-2222-2222-222222222222" {
		t.Errorf("CurrentNodeID = %q, want the 200 current_node_id", res.CurrentNodeID)
	}
	if res.Task.TaskID != "" {
		t.Errorf("Task.TaskID = %q, want empty on no-op", res.Task.TaskID)
	}
}

// TestMigration_Get decodes the full migration view including the nullable
// fields and the scheduling_reason surfaced while pending.
func TestMigration_Get(t *testing.T) {
	t.Parallel()
	reason := "no_eligible_target"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/migrations/33333333-3333-3333-3333-333333333333" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":                "33333333-3333-3333-3333-333333333333",
			"vm_id":             "44444444-4444-4444-4444-444444444444",
			"source_node_id":    "55555555-5555-5555-5555-555555555555",
			"target_node_id":    nil,
			"reason":            "manual",
			"phase":             "pending",
			"progress_percent":  0,
			"scheduling_reason": reason,
			"error_message":     nil,
			"live":              true,
			"allow_postcopy":    false,
			"target_pool_name":  "",
			"created_at":        "2026-06-14T10:00:00Z",
			"updated_at":        "2026-06-14T10:00:01Z",
		})
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	m, raw, err := c.Migration(context.Background(), "33333333-3333-3333-3333-333333333333")
	if err != nil {
		t.Fatalf("Migration: %v", err)
	}
	if m.ID != "33333333-3333-3333-3333-333333333333" {
		t.Errorf("id = %q", m.ID)
	}
	if m.Phase != "pending" {
		t.Errorf("phase = %q, want pending", m.Phase)
	}
	if m.SchedulingReason == nil || *m.SchedulingReason != reason {
		t.Errorf("scheduling_reason = %v, want %q", m.SchedulingReason, reason)
	}
	if m.TargetNodeID != nil {
		t.Errorf("target_node_id = %v, want nil", m.TargetNodeID)
	}
	if len(raw) == 0 {
		t.Errorf("raw body empty, want passthrough bytes")
	}
}

// TestCancelMigration POSTs to /v1/migrations/{id}/cancel and decodes the 200
// Migration view the endpoint returns (the row in its post-cancel phase).
func TestCancelMigration(t *testing.T) {
	t.Parallel()
	const id = "33333333-3333-3333-3333-333333333333"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/migrations/"+id+"/cancel" {
			t.Errorf("path = %s, want /v1/migrations/%s/cancel", r.URL.Path, id)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":               id,
			"vm_id":            "44444444-4444-4444-4444-444444444444",
			"reason":           "manual",
			"phase":            "cancelled",
			"progress_percent": 0,
			"error_message":    "cancelled by operator",
			"live":             true,
			"allow_postcopy":   false,
			"target_pool_name": "",
			"created_at":       "2026-06-14T10:00:00Z",
			"updated_at":       "2026-06-14T10:00:02Z",
		})
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	m, err := c.CancelMigration(context.Background(), id)
	if err != nil {
		t.Fatalf("CancelMigration: %v", err)
	}
	if m.ID != id {
		t.Errorf("id = %q, want %q", m.ID, id)
	}
	if m.Phase != "cancelled" {
		t.Errorf("phase = %q, want cancelled", m.Phase)
	}
}

// TestCancelMigration_NotFound surfaces a 404 as an *APIError (not a silent
// zero value): a missing migration id must reach the caller as an error.
func TestCancelMigration_NotFound(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"migration not found"}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	if _, err := c.CancelMigration(context.Background(), uuid.NewString()); err == nil {
		t.Fatal("CancelMigration: want error for 404, got nil")
	}
}

// TestListMigrations decodes the envelope and surfaces next_cursor + filters.
func TestListMigrations(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/migrations" {
			t.Errorf("path = %s, want /v1/migrations", r.URL.Path)
		}
		if got := r.URL.Query().Get("vm"); got != "vm-uuid" {
			t.Errorf("vm filter = %q, want vm-uuid", got)
		}
		if got := r.URL.Query().Get("node"); got != "node-uuid" {
			t.Errorf("node filter = %q, want node-uuid", got)
		}
		if got := r.URL.Query().Get("limit"); got != "10" {
			t.Errorf("limit = %q, want 10", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{
					"id":               uuid.NewString(),
					"vm_id":            uuid.NewString(),
					"phase":            "active",
					"reason":           "manual",
					"progress_percent": 47,
					"live":             true,
					"created_at":       "2026-06-14T10:00:00Z",
					"updated_at":       "2026-06-14T10:00:00Z",
				},
			},
			"meta": map[string]any{"next_cursor": "CURSOR123"},
		})
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	ms, next, err := c.ListMigrations(context.Background(), cpclient.ListMigrationsParams{
		VM:    "vm-uuid",
		Node:  "node-uuid",
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListMigrations: %v", err)
	}
	if len(ms) != 1 {
		t.Fatalf("len(migrations) = %d, want 1", len(ms))
	}
	if ms[0].ProgressPercent != 47 {
		t.Errorf("progress = %d, want 47", ms[0].ProgressPercent)
	}
	if next != "CURSOR123" {
		t.Errorf("next_cursor = %q, want CURSOR123", next)
	}
}
