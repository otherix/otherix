// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package integration_test

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/agentmock"
	"github.com/otherix/otherix/internal/api"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/store"
)

// cpHarness wires a real Postgres-backed Store, an auth.Service, and
// the api-server router into an httptest.Server so each test can talk
// to the Control Plane the way a real client does (full middleware
// stack, real JSON encoding, real error envelopes).
//
// The agentmock.Mock is intentionally NOT part of this harness — each
// test starts its own fixture so it owns the mock's lifecycle and
// configuration (per the agent-mock design).
type cpHarness struct {
	srv   *httptest.Server
	store *store.Store
	svc   *auth.Service
}

func (h *cpHarness) close() {
	h.srv.Close()
	h.store.Close()
}

// newCPHarness returns a fresh CP harness backed by the package-level
// shared Postgres container. Cleanup is registered via t.Cleanup so
// callers do not have to defer close.
func newCPHarness(t *testing.T) *cpHarness {
	t.Helper()
	if shared == nil {
		t.Fatal("shared harness not initialised — TestMain did not run")
	}
	s, err := store.NewStore(context.Background(), config.DatabaseConfig{
		DSN: shared.DSN, MaxConns: 4, MinConns: 1,
	})
	if err != nil {
		t.Fatalf("store.NewStore: %v", err)
	}

	svc, err := auth.NewService(auth.Config{
		JWTSecret:    []byte("test-secret-32-bytes-padding-pad-"),
		JWTAccessTTL: 15 * time.Minute,
		RefreshTTL:   720 * time.Hour,
	}, s)
	if err != nil {
		t.Fatalf("auth.NewService: %v", err)
	}

	router := api.NewRouter(api.RouterDeps{
		Store:          s,
		AuthService:    svc,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		RequestTimeout: 10 * time.Second,
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	t.Cleanup(s.Close)

	return &cpHarness{srv: srv, store: s, svc: svc}
}

// cpAgentHarness extends cpHarness with the dedicated agent-only mTLS
// listener, wired with the same Store + agentMTLS middleware the
// production NewAgentRouter uses. The TLS material comes from the
// agentmock package (it controls the CA the mock-agent's client cert
// chains to) so a single mock instance can talk to this listener
// end-to-end without bespoke cert plumbing.
type cpAgentHarness struct {
	*cpHarness
	agentSrv *httptest.Server
}

func newCPAgentHarness(t *testing.T) *cpAgentHarness {
	t.Helper()
	base := newCPHarness(t)

	router := api.NewAgentRouter(api.RouterDeps{
		Store:          base.store,
		AuthService:    base.svc,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		RequestTimeout: 10 * time.Second,
	})

	tlsCfg, err := agentmock.ControlPlaneServerTLSConfig()
	if err != nil {
		t.Fatalf("ControlPlaneServerTLSConfig: %v", err)
	}
	srv := httptest.NewUnstartedServer(router)
	srv.TLS = tlsCfg
	srv.StartTLS()
	t.Cleanup(srv.Close)

	return &cpAgentHarness{cpHarness: base, agentSrv: srv}
}
