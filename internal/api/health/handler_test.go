// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package health_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/api/health"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/migrationtest"
	"github.com/otherix/otherix/internal/store"
)

func newStore(t *testing.T, h *migrationtest.Harness) *store.Store {
	t.Helper()
	s, err := store.NewStore(context.Background(), config.DatabaseConfig{
		DSN:      h.DSN,
		MaxConns: 4,
		MinConns: 1,
	})
	if err != nil {
		t.Fatalf("store.NewStore: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestLive_AlwaysOK(t *testing.T) {
	if sharedHarness == nil {
		t.Fatal("sharedHarness not initialised")
	}
	s := newStore(t, sharedHarness)
	handler := health.New(s)

	rec := httptest.NewRecorder()
	handler.Live(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}

	var body health.LiveResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want %q", body.Status, "ok")
	}
	if body.Version == "" {
		t.Error("version should be set from internal/version")
	}
}

func TestReady_DatabaseUp(t *testing.T) {
	if sharedHarness == nil {
		t.Fatal("sharedHarness not initialised")
	}
	s := newStore(t, sharedHarness)
	handler := health.New(s)

	rec := httptest.NewRecorder()
	handler.Ready(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}

	var body health.ReadyResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want %q", body.Status, "ok")
	}
	dbCheck, ok := body.Checks["database"]
	if !ok {
		t.Fatalf("checks.database missing; got: %#v", body.Checks)
	}
	if dbCheck.Status != "ok" {
		t.Errorf("checks.database.status = %q, want %q", dbCheck.Status, "ok")
	}
	if dbCheck.Error != "" {
		t.Errorf("checks.database.error = %q, want empty", dbCheck.Error)
	}
}

func TestReady_DatabaseDown(t *testing.T) {
	h := migrationtest.MustStart(t)
	s := newStore(t, h)

	// Stop the underlying container so subsequent Ping fails. Using a
	// fresh Harness keeps other tests from breaking.
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h.Stop(stopCtx)

	handler := health.New(s)

	rec := httptest.NewRecorder()
	handler.Ready(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}

	var body health.ReadyResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Status != "not_ready" {
		t.Errorf("status = %q, want %q", body.Status, "not_ready")
	}
	dbCheck, ok := body.Checks["database"]
	if !ok {
		t.Fatalf("checks.database missing; got: %#v", body.Checks)
	}
	if dbCheck.Status != "fail" {
		t.Errorf("checks.database.status = %q, want %q", dbCheck.Status, "fail")
	}
	if dbCheck.Error == "" {
		t.Error("checks.database.error should be populated when DB ping fails")
	}
}
