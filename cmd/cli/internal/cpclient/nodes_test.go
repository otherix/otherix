// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cpclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

func TestListNodes_FullView(t *testing.T) {
	t.Parallel()
	nodeID := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/v1/nodes" {
			t.Errorf("path = %s, want /v1/nodes", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want Bearer test-token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{
					"id":                  nodeID,
					"name":                "node-a",
					"architecture":        "arm64",
					"advertised_endpoint": "https://node-a.example.local:8443",
					"migration": map[string]any{
						"host":             "node-a.internal",
						"port_range_start": 49152,
						"port_range_end":   49251,
					},
					"status":            "ready",
					"cordoned_at":       nil,
					"cpu_cores_total":   32,
					"memory_total_mib":  int64(65536),
					"cpu_flags":         []string{"vmx"},
					"numa_topology":     nil,
					"capabilities":      map[string]any{},
					"last_heartbeat_at": "2026-05-11T10:00:00Z",
					"labels":            map[string]string{"zone": "us-east-1a"},
					"created_at":        "2026-05-10T10:00:00Z",
					"updated_at":        "2026-05-11T10:00:00Z",
				},
			},
			"meta": map[string]any{"next_cursor": nil},
		})
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.ListNodes(context.Background(), cpclient.ListNodesParams{})
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(got.Data) != 1 {
		t.Fatalf("len(Data) = %d, want 1", len(got.Data))
	}
	if got.Data[0].Name != "node-a" {
		t.Errorf("Name = %s, want node-a", got.Data[0].Name)
	}
	if got.Data[0].Migration == nil || got.Data[0].Migration.Host != "node-a.internal" {
		t.Errorf("Migration = %+v, want host=node-a.internal", got.Data[0].Migration)
	}
	if got.Data[0].CPUCoresTotal == nil || *got.Data[0].CPUCoresTotal != 32 {
		t.Errorf("CPUCoresTotal = %v, want 32", got.Data[0].CPUCoresTotal)
	}
}

func TestListNodes_SummaryView(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{
					"id":           uuid.NewString(),
					"name":         "node-a",
					"architecture": "arm64",
					"status":       "ready",
					"cordoned_at":  nil,
					"labels":       map[string]string{},
					"created_at":   "2026-05-10T10:00:00Z",
				},
			},
			"meta": map[string]any{"next_cursor": nil},
		})
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.ListNodes(context.Background(), cpclient.ListNodesParams{})
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(got.Data) != 1 {
		t.Fatalf("len(Data) = %d, want 1", len(got.Data))
	}
	if got.Data[0].Migration != nil {
		t.Errorf("Migration = %+v, want nil for summary view", got.Data[0].Migration)
	}
	if got.Data[0].AdvertisedEndpoint != nil {
		t.Errorf("AdvertisedEndpoint = %v, want nil for summary view", got.Data[0].AdvertisedEndpoint)
	}
	if got.Data[0].CPUCoresTotal != nil {
		t.Errorf("CPUCoresTotal = %v, want nil for summary view", got.Data[0].CPUCoresTotal)
	}
}

func TestListNodes_FiltersEncoded(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("architecture") != "amd64" {
			t.Errorf("architecture = %q, want amd64", q.Get("architecture"))
		}
		if q.Get("status") != "ready" {
			t.Errorf("status = %q, want ready", q.Get("status"))
		}
		if q.Get("limit") != "25" {
			t.Errorf("limit = %q, want 25", q.Get("limit"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[],"meta":{"next_cursor":null}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	if _, err := c.ListNodes(context.Background(), cpclient.ListNodesParams{
		Limit:        25,
		Architecture: "amd64",
		Status:       "ready",
	}); err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
}

func TestListNodes_Pagination(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("cursor"); got != "abc" {
			t.Errorf("cursor = %q, want abc", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{},
			"meta": map[string]any{"next_cursor": "def"},
		})
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.ListNodes(context.Background(), cpclient.ListNodesParams{Cursor: "abc"})
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if got.Meta.NextCursor == nil || *got.Meta.NextCursor != "def" {
		t.Errorf("NextCursor = %v, want def", got.Meta.NextCursor)
	}
}

func TestGetNode_ByName(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/nodes/node-a" {
			t.Errorf("path = %s, want /v1/nodes/node-a", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"00000000-0000-0000-0000-000000000001","name":"node-a","architecture":"arm64","status":"ready","labels":{},"created_at":"2026-05-10T10:00:00Z"}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.GetNode(context.Background(), "node-a")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if got.Name != "node-a" {
		t.Errorf("Name = %s, want node-a", got.Name)
	}
}

func TestGetNode_404(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"node not found"}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	_, err := c.GetNode(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected error for 404")
	}
	var ae *cpclient.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if ae.Code != "not_found" {
		t.Errorf("Code = %s, want not_found", ae.Code)
	}
}

func TestGetNode_Cordoned(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"00000000-0000-0000-0000-000000000001","name":"node-a","architecture":"arm64","status":"cordoned","cordoned_at":"2026-05-11T10:00:00Z","labels":{},"created_at":"2026-05-10T10:00:00Z"}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.GetNode(context.Background(), "node-a")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if got.CordonedAt == nil || *got.CordonedAt == "" {
		t.Errorf("CordonedAt = %v, want non-empty", got.CordonedAt)
	}
	if got.Status != "cordoned" {
		t.Errorf("Status = %s, want cordoned", got.Status)
	}
}

func TestGetNode_MalformedJSON(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{not valid`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	_, err := c.GetNode(context.Background(), "node-a")
	if err == nil {
		t.Fatal("expected decode error")
	}
}
