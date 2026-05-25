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
