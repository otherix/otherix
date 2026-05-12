// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/otherix/otherix/internal/api"
	vmshandlers "github.com/otherix/otherix/internal/api/handlers/vms"
	"github.com/otherix/otherix/internal/api/health"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/migrationtest"
	"github.com/otherix/otherix/internal/store"
)

func newAuthService(t *testing.T, s *store.Store) *auth.Service {
	t.Helper()
	svc, err := auth.NewService(auth.Config{
		JWTSecret:    []byte("test-secret-32-bytes-padding-pad-"),
		JWTAccessTTL: 15 * time.Minute,
		RefreshTTL:   720 * time.Hour,
	}, s)
	if err != nil {
		t.Fatalf("auth.NewService: %v", err)
	}
	return svc
}

// newRiverClientForServerTest builds a river client without Start —
// the server tests don't drive workers, only construct the HTTP layer.
// JobCancelTx (used by tasks.cancel) works on river_job directly via
// the supplied tx and does not require Start.
func newRiverClientForServerTest(t *testing.T, s *store.Store) *river.Client[pgx.Tx] {
	t.Helper()
	c, err := api.BuildRiverClient(api.RiverDeps{
		Pool:   s.Pool(),
		Cfg:    config.WorkersConfig{Enabled: false, MaxWorkers: 1},
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Store:  s,
	})
	if err != nil {
		t.Fatalf("BuildRiverClient: %v", err)
	}
	return c
}

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

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return addr
}

func TestServer_StartsAndShutsDown(t *testing.T) {
	if sharedHarness == nil {
		t.Fatal("sharedHarness not initialised")
	}
	s := newStore(t, sharedHarness)

	addr := freePort(t)
	cfg := config.APIConfig{
		Server: config.ServerConfig{
			Listen:        addr,
			ReadTimeout:   5 * time.Second,
			WriteTimeout:  5 * time.Second,
			ShutdownGrace: 5 * time.Second,
		},
	}
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))

	server, err := api.NewServer(cfg, s, newRiverClientForServerTest(t, s), nil, vmshandlers.LifecycleDeps{}, vmshandlers.ConsoleDeps{}, newAuthService(t, s), api.TLSMaterial{}, log)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- server.Run(ctx) }()

	// Wait for the server to come up by polling /healthz.
	deadline := time.Now().Add(3 * time.Second)
	var resp *http.Response
	for time.Now().Before(deadline) {
		r, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			resp = r
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if resp == nil {
		cancel()
		t.Fatal("server never came up")
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz status = %d, want 200", resp.StatusCode)
	}
	var live health.LiveResponse
	if err := json.NewDecoder(resp.Body).Decode(&live); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if live.Status != "ok" {
		t.Errorf("status = %q, want ok", live.Status)
	}

	// Trigger graceful shutdown.
	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Errorf("Run returned: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return within 10 s of cancel")
	}
}

func TestServer_ListenError(t *testing.T) {
	if sharedHarness == nil {
		t.Fatal("sharedHarness not initialised")
	}
	s := newStore(t, sharedHarness)

	// Take a port, then ask the server to bind to it — it must fail.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	addr := l.Addr().String()

	cfg := config.APIConfig{
		Server: config.ServerConfig{
			Listen:        addr,
			ReadTimeout:   5 * time.Second,
			WriteTimeout:  5 * time.Second,
			ShutdownGrace: 1 * time.Second,
		},
	}
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	server, err := api.NewServer(cfg, s, newRiverClientForServerTest(t, s), nil, vmshandlers.LifecycleDeps{}, vmshandlers.ConsoleDeps{}, newAuthService(t, s), api.TLSMaterial{}, log)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	if err := server.Run(context.Background()); err == nil {
		t.Fatal("Run returned nil for bind-on-busy-port; want error")
	}
}
