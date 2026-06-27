// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cpclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

func TestGetClusterDefaultPool_Set(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/v1/cluster/default-pool" {
			t.Errorf("path = %s, want /v1/cluster/default-pool", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want Bearer test-token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"default"}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.GetClusterDefaultPool(context.Background())
	if err != nil {
		t.Fatalf("GetClusterDefaultPool: %v", err)
	}
	if got == nil {
		t.Fatal("got = nil, want non-nil")
	}
	if got.Name != "default" {
		t.Errorf("Name = %s, want default", got.Name)
	}
}

func TestGetClusterDefaultPool_UnsetReturnsNilNoError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"default_pool_not_set","message":"unset"}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.GetClusterDefaultPool(context.Background())
	if err != nil {
		t.Fatalf("expected nil error for unset default, got %v", err)
	}
	if got != nil {
		t.Errorf("got = %+v, want nil", got)
	}
}

func TestGetClusterDefaultPool_OtherErrorSurfaces(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"db down"}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	_, err := c.GetClusterDefaultPool(context.Background())
	if err == nil {
		t.Fatal("expected error for 500")
	}
	var ae *cpclient.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if ae.Code != "internal" {
		t.Errorf("Code = %s, want internal", ae.Code)
	}
}

func TestSetClusterDefaultPool_HappyPath(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		var got cpclient.ClusterDefaultPool
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if got.Name != "fast-ssd" {
			t.Errorf("body.name = %s, want fast-ssd", got.Name)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"fast-ssd"}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.SetClusterDefaultPool(context.Background(), "fast-ssd")
	if err != nil {
		t.Fatalf("SetClusterDefaultPool: %v", err)
	}
	if got.Name != "fast-ssd" {
		t.Errorf("Name = %s, want fast-ssd", got.Name)
	}
}

func TestSetClusterDefaultPool_UnknownPoolName(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"pool_not_found","message":"no such pool"}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	_, err := c.SetClusterDefaultPool(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected error for unknown pool")
	}
	var ae *cpclient.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if ae.Code != "pool_not_found" {
		t.Errorf("Code = %s, want pool_not_found", ae.Code)
	}
}

func TestClearClusterDefaultPool_HappyPath(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/v1/cluster/default-pool") {
			t.Errorf("path = %s, want suffix /v1/cluster/default-pool", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	if err := c.ClearClusterDefaultPool(context.Background()); err != nil {
		t.Fatalf("ClearClusterDefaultPool: %v", err)
	}
}

func TestGetClusterDefaultNetwork_Set(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/cluster/default-network" || r.Method != http.MethodGet {
			t.Errorf("got %s %s, want GET /v1/cluster/default-network", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"qnet"}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.GetClusterDefaultNetwork(context.Background())
	if err != nil {
		t.Fatalf("GetClusterDefaultNetwork() error = %v", err)
	}
	if got == nil || got.Name != "qnet" {
		t.Errorf("GetClusterDefaultNetwork() = %v, want {qnet}", got)
	}
}

func TestGetClusterDefaultNetwork_UnsetReturnsNilNoError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"default_network_not_set","message":"unset"}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.GetClusterDefaultNetwork(context.Background())
	if err != nil {
		t.Fatalf("GetClusterDefaultNetwork() error = %v", err)
	}
	if got != nil {
		t.Errorf("GetClusterDefaultNetwork() = %v, want nil for unset", got)
	}
}

func TestSetClusterDefaultNetwork_HappyPath(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		var got cpclient.ClusterDefaultPool
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if got.Name != "qnet" {
			t.Errorf("body.name = %s, want qnet", got.Name)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"qnet"}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.SetClusterDefaultNetwork(context.Background(), "qnet")
	if err != nil {
		t.Fatalf("SetClusterDefaultNetwork() error = %v", err)
	}
	if got.Name != "qnet" {
		t.Errorf("Name = %s, want qnet", got.Name)
	}
}

func TestClearClusterDefaultNetwork_HappyPath(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/v1/cluster/default-network") {
			t.Errorf("path = %s, want suffix /v1/cluster/default-network", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	if err := c.ClearClusterDefaultNetwork(context.Background()); err != nil {
		t.Fatalf("ClearClusterDefaultNetwork() error = %v", err)
	}
}

func TestListClusterMembers_HappyPath(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/v1/cluster/members" {
			t.Errorf("path = %s, want /v1/cluster/members", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want Bearer test-token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"abc123","name":"replica-0","peer_urls":["https://10.0.0.1:2380"],"client_urls":["https://10.0.0.1:2379"],"is_learner":false},{"id":"def456","name":"replica-1","peer_urls":["https://10.0.0.2:2380"],"client_urls":["https://10.0.0.2:2379"],"is_learner":true}]}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.ListClusterMembers(context.Background())
	if err != nil {
		t.Fatalf("ListClusterMembers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(members) = %d, want 2", len(got))
	}
	if got[0].ID != "abc123" {
		t.Errorf("members[0].ID = %s, want abc123", got[0].ID)
	}
	if got[0].Name != "replica-0" {
		t.Errorf("members[0].Name = %s, want replica-0", got[0].Name)
	}
	if len(got[0].PeerURLs) != 1 || got[0].PeerURLs[0] != "https://10.0.0.1:2380" {
		t.Errorf("members[0].PeerURLs = %v, want [https://10.0.0.1:2380]", got[0].PeerURLs)
	}
	if got[0].IsLearner {
		t.Errorf("members[0].IsLearner = true, want false")
	}
	if !got[1].IsLearner {
		t.Errorf("members[1].IsLearner = false, want true")
	}
}

func TestListClusterMembers_Empty(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.ListClusterMembers(context.Background())
	if err != nil {
		t.Fatalf("ListClusterMembers: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len(members) = %d, want 0", len(got))
	}
}

func TestListClusterMembers_APIError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"permission_denied","message":"admin only"}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	_, err := c.ListClusterMembers(context.Background())
	if err == nil {
		t.Fatal("expected error for 403")
	}
	var ae *cpclient.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if ae.Code != "permission_denied" {
		t.Errorf("Code = %s, want permission_denied", ae.Code)
	}
}

func TestRemoveClusterMember_HappyPath(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		if r.URL.Path != "/v1/cluster/members/abc123" {
			t.Errorf("path = %s, want /v1/cluster/members/abc123", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want Bearer test-token", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	if err := c.RemoveClusterMember(context.Background(), "abc123"); err != nil {
		t.Fatalf("RemoveClusterMember: %v", err)
	}
}

func TestRemoveClusterMember_NotFound(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"validation_failed","message":"member not found"}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	err := c.RemoveClusterMember(context.Background(), "deadbeef")
	if err == nil {
		t.Fatal("expected error for 400")
	}
	var ae *cpclient.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if ae.Code != "validation_failed" {
		t.Errorf("Code = %s, want validation_failed", ae.Code)
	}
}
