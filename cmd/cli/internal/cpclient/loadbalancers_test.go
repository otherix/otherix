// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cpclient_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// TestCreateLoadBalancer_HappyPath asserts CreateLoadBalancer POSTs to
// /v1/loadbalancers with the name/port/selector body and decodes the
// 201 LoadBalancer view.
func TestCreateLoadBalancer_HappyPath(t *testing.T) {
	t.Parallel()
	lbID := uuid.NewString()
	ownerID := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/loadbalancers" {
			t.Errorf("path = %s, want /v1/loadbalancers", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		want := map[string]any{
			"name": "web",
			"port": float64(8080),
			"selector": map[string]any{
				"app":  "web",
				"tier": "fe",
			},
		}
		if diff := cmp.Diff(want, body); diff != "" {
			t.Errorf("request body mismatch (-want +got):\n%s", diff)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":         lbID,
			"name":       "web",
			"owner_id":   ownerID,
			"port":       8080,
			"selector":   map[string]any{"app": "web", "tier": "fe"},
			"created_at": "2026-07-01T10:00:00Z",
			"updated_at": "2026-07-01T10:00:00Z",
		})
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.CreateLoadBalancer(context.Background(), cpclient.CreateLoadBalancerParams{
		Name:     "web",
		Port:     8080,
		Selector: map[string]string{"app": "web", "tier": "fe"},
	})
	if err != nil {
		t.Fatalf("CreateLoadBalancer: %v", err)
	}
	if got.ID != lbID {
		t.Errorf("ID = %s, want %s", got.ID, lbID)
	}
	if got.Name != "web" {
		t.Errorf("Name = %s, want web", got.Name)
	}
	if got.Port != 8080 {
		t.Errorf("Port = %d, want 8080", got.Port)
	}
	if diff := cmp.Diff(map[string]string{"app": "web", "tier": "fe"}, got.Selector); diff != "" {
		t.Errorf("Selector mismatch (-want +got):\n%s", diff)
	}
}

// TestGetLoadBalancer_ByName asserts GetLoadBalancer addresses the CP
// route by name directly (the {id} path param is the load-balancer
// name; no client-side name->uuid resolution) and returns both the
// decoded value and the raw body.
func TestGetLoadBalancer_ByName(t *testing.T) {
	t.Parallel()
	ownerID := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/v1/loadbalancers/web" {
			t.Errorf("path = %s, want /v1/loadbalancers/web", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":         uuid.NewString(),
			"name":       "web",
			"owner_id":   ownerID,
			"port":       443,
			"selector":   map[string]any{"app": "web"},
			"created_at": "2026-07-01T10:00:00Z",
			"updated_at": "2026-07-01T10:00:00Z",
		})
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, raw, err := c.GetLoadBalancer(context.Background(), "web")
	if err != nil {
		t.Fatalf("GetLoadBalancer: %v", err)
	}
	if got.Name != "web" {
		t.Errorf("Name = %s, want web", got.Name)
	}
	if got.Port != 443 {
		t.Errorf("Port = %d, want 443", got.Port)
	}
	if len(raw) == 0 {
		t.Errorf("raw body is empty, want the server projection verbatim")
	}
}

// TestListLoadBalancers_HappyPath asserts ListLoadBalancers hits
// /v1/loadbalancers with the pagination query and decodes the page.
func TestListLoadBalancers_HappyPath(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/loadbalancers" {
			t.Errorf("path = %s, want /v1/loadbalancers", r.URL.Path)
		}
		if got := r.URL.Query().Get("limit"); got != "25" {
			t.Errorf("limit = %q, want 25", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{
					"id":         uuid.NewString(),
					"name":       "web",
					"owner_id":   uuid.NewString(),
					"port":       8080,
					"selector":   map[string]any{"app": "web"},
					"created_at": "2026-07-01T10:00:00Z",
					"updated_at": "2026-07-01T10:00:00Z",
				},
			},
			"meta": map[string]any{"next_cursor": nil},
		})
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.ListLoadBalancers(context.Background(), cpclient.ListLoadBalancersParams{Limit: 25})
	if err != nil {
		t.Fatalf("ListLoadBalancers: %v", err)
	}
	if len(got.Data) != 1 {
		t.Fatalf("len(Data) = %d, want 1", len(got.Data))
	}
	if got.Data[0].Name != "web" {
		t.Errorf("Data[0].Name = %s, want web", got.Data[0].Name)
	}
}

// TestUpdateLoadBalancer_PatchesByName asserts UpdateLoadBalancer PATCHes
// /v1/loadbalancers/{name} with only the set fields.
func TestUpdateLoadBalancer_PatchesByName(t *testing.T) {
	t.Parallel()
	port := int32(9090)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s, want PATCH", r.Method)
		}
		if r.URL.Path != "/v1/loadbalancers/web" {
			t.Errorf("path = %s, want /v1/loadbalancers/web", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		want := map[string]any{"port": float64(9090)}
		if diff := cmp.Diff(want, body); diff != "" {
			t.Errorf("request body mismatch (-want +got):\n%s", diff)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":         uuid.NewString(),
			"name":       "web",
			"owner_id":   uuid.NewString(),
			"port":       9090,
			"selector":   map[string]any{"app": "web"},
			"created_at": "2026-07-01T10:00:00Z",
			"updated_at": "2026-07-01T11:00:00Z",
		})
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.UpdateLoadBalancer(context.Background(), "web", cpclient.UpdateLoadBalancerParams{Port: &port})
	if err != nil {
		t.Fatalf("UpdateLoadBalancer: %v", err)
	}
	if got.Port != 9090 {
		t.Errorf("Port = %d, want 9090", got.Port)
	}
}

// TestDeleteLoadBalancer_ByName asserts DeleteLoadBalancer issues a
// DELETE to /v1/loadbalancers/{name} and treats 204 as success.
func TestDeleteLoadBalancer_ByName(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		if r.URL.Path != "/v1/loadbalancers/web" {
			t.Errorf("path = %s, want /v1/loadbalancers/web", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	if err := c.DeleteLoadBalancer(context.Background(), "web"); err != nil {
		t.Fatalf("DeleteLoadBalancer: %v", err)
	}
}
