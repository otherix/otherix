// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cpclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

func TestListPools_HappyPath(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/v1/storage-pools" {
			t.Errorf("path = %s, want /v1/storage-pools", r.URL.Path)
		}
		if got := r.URL.Query().Get("node"); got != "node-a" {
			t.Errorf("node filter = %q, want node-a", got)
		}
		if got := r.URL.Query().Get("limit"); got != "25" {
			t.Errorf("limit = %q, want 25", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want Bearer test-token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{
					"id":                 uuid.NewString(),
					"node":               "node-a",
					"name":               "default",
					"type":               "local_dir",
					"path":               "/var/lib/otherix/pools/default",
					"is_cluster_default": true,
					"config":             map[string]any{},
					"created_at":         "2026-05-11T10:00:00Z",
					"updated_at":         "2026-05-11T10:00:00Z",
				},
			},
			"meta": map[string]any{"next_cursor": nil},
		})
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.ListPools(context.Background(), cpclient.ListPoolsParams{Node: "node-a", Limit: 25})
	if err != nil {
		t.Fatalf("ListPools: %v", err)
	}
	if len(got.Data) != 1 {
		t.Fatalf("len(Data) = %d, want 1", len(got.Data))
	}
	if !got.Data[0].IsClusterDefault {
		t.Errorf("IsClusterDefault = false, want true")
	}
	if got.Data[0].Node != "node-a" {
		t.Errorf("Node = %s, want node-a", got.Data[0].Node)
	}
}

func TestListPools_Pagination(t *testing.T) {
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
	got, err := c.ListPools(context.Background(), cpclient.ListPoolsParams{Cursor: "abc"})
	if err != nil {
		t.Fatalf("ListPools: %v", err)
	}
	if got.Meta.NextCursor == nil || *got.Meta.NextCursor != "def" {
		t.Errorf("NextCursor = %v, want def", got.Meta.NextCursor)
	}
}

func TestGetPoolByID_HappyPath(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/storage-pools/"+id.String() {
			t.Errorf("path = %s, want /v1/storage-pools/{uuid}", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":                 id.String(),
			"node":               "node-a",
			"name":               "default",
			"type":               "local_dir",
			"path":               "/var/lib/otherix/pools/default",
			"is_cluster_default": false,
			"config":             map[string]any{},
			"created_at":         "2026-05-11T10:00:00Z",
			"updated_at":         "2026-05-11T10:00:00Z",
		})
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, _, err := c.GetPoolByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetPoolByID: %v", err)
	}
	if got.ID != id {
		t.Errorf("ID = %s, want %s", got.ID, id)
	}
}

func TestGetPoolByID_404(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"storage pool not found"}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	_, _, err := c.GetPoolByID(context.Background(), uuid.New())
	if err == nil {
		t.Fatalf("expected error for 404")
	}
	var ae *cpclient.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if ae.Code != "not_found" {
		t.Errorf("Code = %s, want not_found", ae.Code)
	}
}

func TestGetPoolByName_AggregatedView(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/storage-pools/default") {
			t.Errorf("path = %s, want suffix /storage-pools/default", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name":               "default",
			"type":               "local_dir",
			"is_cluster_default": true,
			"instances": []map[string]any{
				{
					"id":                 uuid.NewString(),
					"node":               "node-a",
					"name":               "default",
					"type":               "local_dir",
					"path":               "/var/lib/otherix/pools/default",
					"is_cluster_default": true,
					"config":             map[string]any{},
					"created_at":         "2026-05-11T10:00:00Z",
					"updated_at":         "2026-05-11T10:00:00Z",
				},
				{
					"id":                 uuid.NewString(),
					"node":               "node-b",
					"name":               "default",
					"type":               "local_dir",
					"path":               "/var/lib/otherix/pools/default",
					"is_cluster_default": true,
					"config":             map[string]any{},
					"created_at":         "2026-05-11T10:00:00Z",
					"updated_at":         "2026-05-11T10:00:00Z",
				},
			},
		})
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, _, err := c.GetPoolByName(context.Background(), "default")
	if err != nil {
		t.Fatalf("GetPoolByName: %v", err)
	}
	if !got.IsClusterDefault {
		t.Errorf("IsClusterDefault = false, want true")
	}
	if len(got.Instances) != 2 {
		t.Errorf("len(Instances) = %d, want 2", len(got.Instances))
	}
}

func TestGetPoolByName_PathEscapesSpecialChars(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// chi receives the decoded value; we assert here on the
		// post-decode form. The escaping is a property of the URL the
		// transport writes — exercised by net/url's round-trip.
		if r.URL.Path != "/v1/storage-pools/pool with space" {
			t.Errorf("decoded path = %q, want /v1/storage-pools/pool with space", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name":               "pool with space",
			"type":               "local_dir",
			"is_cluster_default": false,
			"instances":          []map[string]any{},
		})
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	if _, _, err := c.GetPoolByName(context.Background(), "pool with space"); err != nil {
		t.Fatalf("GetPoolByName: %v", err)
	}
}

func TestCreatePool_HappyPath(t *testing.T) {
	t.Parallel()
	poolID := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/storage-pools" {
			t.Errorf("path = %s, want /v1/storage-pools", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["node"] != "node-mvp" {
			t.Errorf("node = %v, want node-mvp", body["node"])
		}
		if body["name"] != "pool-mvp" {
			t.Errorf("name = %v, want pool-mvp", body["name"])
		}
		if body["type"] != "local_dir" {
			t.Errorf("type = %v, want local_dir", body["type"])
		}
		if body["path"] != "/opt/otherix/pools/default" {
			t.Errorf("path = %v, want /opt/otherix/pools/default", body["path"])
		}
		if _, present := body["config"]; present {
			t.Errorf("config key must not appear when caller omitted it (server applies default)")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":                 poolID,
			"node":               "node-mvp",
			"name":               "pool-mvp",
			"type":               "local_dir",
			"path":               "/opt/otherix/pools/default",
			"is_cluster_default": false,
			"config":             map[string]any{},
			"created_at":         "2026-05-15T10:00:00Z",
			"updated_at":         "2026-05-15T10:00:00Z",
		})
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.CreatePool(context.Background(), cpclient.CreatePoolRequest{
		Node: "node-mvp",
		Name: "pool-mvp",
		Type: "local_dir",
		Path: "/opt/otherix/pools/default",
	})
	if err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	if got.Name != "pool-mvp" {
		t.Errorf("Name = %s, want pool-mvp", got.Name)
	}
	if got.Node != "node-mvp" {
		t.Errorf("Node = %s, want node-mvp", got.Node)
	}
}

func TestCreatePool_ConfigSerialisedWhenSet(t *testing.T) {
	t.Parallel()
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"00000000-0000-0000-0000-000000000001","node":"node-a","name":"p","type":"local_dir","path":"/x","is_cluster_default":false,"config":{"k":"v"},"created_at":"2026-05-15T10:00:00Z","updated_at":"2026-05-15T10:00:00Z"}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	_, err := c.CreatePool(context.Background(), cpclient.CreatePoolRequest{
		Node:   "node-a",
		Name:   "p",
		Type:   "local_dir",
		Path:   "/x",
		Config: map[string]interface{}{"k": "v"},
	})
	if err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	cfg, ok := captured["config"].(map[string]any)
	if !ok {
		t.Fatalf("config not serialised as object: %T", captured["config"])
	}
	if cfg["k"] != "v" {
		t.Errorf("config.k = %v, want v", cfg["k"])
	}
}

func TestCreatePool_409ReturnsSentinel(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"another storage pool with this name already exists on the target node","details":{"field":"name"}}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	_, err := c.CreatePool(context.Background(), cpclient.CreatePoolRequest{
		Node: "node-mvp", Name: "pool-mvp", Type: "local_dir", Path: "/opt/otherix/pools/default",
	})
	if !errors.Is(err, cpclient.ErrPoolExists) {
		t.Fatalf("err = %v, want errors.Is(err, ErrPoolExists)", err)
	}
}

func TestCreatePool_404NodeMissing(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"node not found"}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	_, err := c.CreatePool(context.Background(), cpclient.CreatePoolRequest{
		Node: "missing", Name: "p", Type: "local_dir", Path: "/x",
	})
	var apiErr *cpclient.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Code != "not_found" {
		t.Errorf("Code = %s, want not_found", apiErr.Code)
	}
}

func TestCreatePool_400Validation(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"validation_failed","message":"path is required"}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	_, err := c.CreatePool(context.Background(), cpclient.CreatePoolRequest{
		Node: "node-mvp", Name: "p", Type: "local_dir",
	})
	var apiErr *cpclient.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Code != "validation_failed" {
		t.Errorf("Code = %s, want validation_failed", apiErr.Code)
	}
}

func TestDeletePool_HappyByName(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		if r.URL.Path != "/v1/storage-pools/pool-mvp" {
			t.Errorf("path = %s, want /v1/storage-pools/pool-mvp", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	if err := c.DeletePool(context.Background(), "pool-mvp"); err != nil {
		t.Fatalf("DeletePool: %v", err)
	}
}

func TestDeletePool_HappyByUUID(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/storage-pools/"+id.String() {
			t.Errorf("path = %s, want /v1/storage-pools/<uuid>", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	if err := c.DeletePool(context.Background(), id.String()); err != nil {
		t.Fatalf("DeletePool: %v", err)
	}
}

func TestDeletePool_BlockedByStorageImages(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"resource_in_use","message":"storage pool still has materialised storage images; delete them first","details":{"blocking_resources":{"storage_images":3,"vm_disks":1},"kind":"pool"}}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	err := c.DeletePool(context.Background(), "pool-mvp")
	var blocked *cpclient.ErrPoolBlocked
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %v, want *ErrPoolBlocked", err)
	}
	if blocked.Code != "resource_in_use" {
		t.Errorf("Code = %s, want resource_in_use", blocked.Code)
	}
	if blocked.Resources["storage_images"] != 3 {
		t.Errorf("storage_images = %d, want 3", blocked.Resources["storage_images"])
	}
	if blocked.Resources["vm_disks"] != 1 {
		t.Errorf("vm_disks = %d, want 1", blocked.Resources["vm_disks"])
	}
}

func TestDeletePool_BlockedByVMDisksOnly(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"storage pool is in use by virtual machine disks; remove or migrate them first","details":{"blocking_resources":{"vm_disks":2}}}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	err := c.DeletePool(context.Background(), "pool-mvp")
	var blocked *cpclient.ErrPoolBlocked
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %v, want *ErrPoolBlocked", err)
	}
	if blocked.Code != "conflict" {
		t.Errorf("Code = %s, want conflict", blocked.Code)
	}
	if blocked.Resources["vm_disks"] != 2 {
		t.Errorf("vm_disks = %d, want 2", blocked.Resources["vm_disks"])
	}
	if _, present := blocked.Resources["storage_images"]; present {
		t.Errorf("storage_images key should not appear when only vm_disks block")
	}
}

func TestDeletePool_404(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"storage pool not found"}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	err := c.DeletePool(context.Background(), "missing")
	var apiErr *cpclient.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Code != "not_found" {
		t.Errorf("Code = %s, want not_found", apiErr.Code)
	}
}

func TestDeletePool_409WithoutBlockingResources(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"something else"}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	err := c.DeletePool(context.Background(), "anything")
	var blocked *cpclient.ErrPoolBlocked
	if errors.As(err, &blocked) {
		t.Fatalf("err = %v, want plain *APIError (no blocking_resources in payload)", err)
	}
	var apiErr *cpclient.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError fallback", err)
	}
}
