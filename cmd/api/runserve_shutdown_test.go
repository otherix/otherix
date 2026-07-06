// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
)

// freePort returns an OS-assigned free TCP port on the loopback interface. It is
// used to give the embedded etcd member non-colliding peer / client URLs.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// runServeTestDeps starts an in-process etcd member and builds the etcdstore,
// auth service, and cluster CA material runServe needs. Everything is torn down
// via t.Cleanup. It mirrors the tests/apie2e harness but stays local to the
// cmd/api package so the test can call the unexported runServe directly.
func runServeTestDeps(t *testing.T) (*etcdstore.Store, *auth.Service, auth.ClusterCAResult, string) {
	t.Helper()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))

	clientURL := fmt.Sprintf("http://127.0.0.1:%d", freePort(t))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rt, err := etcd.Start(ctx, &etcd.Config{
		Mode:          etcd.ModeSingle,
		Name:          "cmdapi-runserve",
		DataDir:       filepath.Join(t.TempDir(), "member"),
		PeerURL:       fmt.Sprintf("http://127.0.0.1:%d", freePort(t)),
		ClientURL:     clientURL,
		ClusterToken:  "otherix-cmdapi-runserve",
		UnsafeNoFsync: true,
	}, discard)
	if err != nil {
		t.Fatalf("etcd.Start: %v", err)
	}
	cli := etcd.NewClient(rt)
	t.Cleanup(func() {
		_ = cli.Close()
		rt.Stop(10 * time.Second)
	})

	st := etcdstore.New(cli)

	svc, err := auth.NewService(auth.Config{
		JWTSecret:    []byte("cmdapi-runserve-secret-32-bytes!"),
		JWTAccessTTL: 15 * time.Minute,
		RefreshTTL:   720 * time.Hour,
	}, st)
	if err != nil {
		t.Fatalf("auth.NewService: %v", err)
	}

	ca, err := auth.GenerateClusterCA(time.Now())
	if err != nil {
		t.Fatalf("GenerateClusterCA: %v", err)
	}
	return st, svc, ca, clientURL
}

// runServeTestConfig returns the minimal config that drives runServe past every
// pre-server step (bootstrap hooks -> skipped CP cert -> nil agent client ->
// no-op workers) so the promote loop launches and the HTTP listener is the only
// thing that can fail. Workers / agent client / agent server / user TLS are all
// disabled, which makes LoadOrGenerateCPCert return skipped material and
// startWorkers a no-op closure. listen is the pre-bound address that forces
// server.Run to fail with EADDRINUSE.
func runServeTestConfig(listen, clientURL string) *config.APIConfig {
	cfg := &config.APIConfig{}
	cfg.Server.Listen = listen
	cfg.Server.ReadTimeout = 5 * time.Second
	cfg.Server.WriteTimeout = 5 * time.Second
	cfg.Server.ShutdownGrace = 5 * time.Second
	cfg.Server.TLS.Enabled = false
	cfg.Workers.Enabled = false
	cfg.AgentClient.Enabled = false
	cfg.AgentServer.Enabled = false
	cfg.Console.AccessMode = "proxy"
	cfg.Etcd.ClientURL = clientURL
	return cfg
}

// TestRunServeDrainsBackgroundLoopsOnServerStartFailure is the seam test for the
// server-start failure path: server.Run returns a listener-startup error WITHOUT
// cancelling ctx, so the dispatcher / scheduler / promote loop - launched under
// the request ctx - would otherwise leak and race the etcd teardown that
// serve.go's deferred Close/Stop performs once runServe returns.
//
// The fix derives a cancelable child ctx for the background loops and drains
// them (cancel + wait) on every post-launch return path. The test forces the
// listener failure by pre-binding cfg.Server.Listen and asserts (1) runServe
// returns a non-nil "server:" error within a bounded deadline (a naive
// stopWorkers without the cancel would deadlock here, since ctx is never
// cancelled on this path), and (2) the goroutine count settles back to the
// pre-launch baseline afterwards - the promote loop was drained, not leaked.
func TestRunServeDrainsBackgroundLoopsOnServerStartFailure(t *testing.T) {
	st, svc, ca, clientURL := runServeTestDeps(t)

	// Ensure the bootstrap-admin env is unset so BootstrapAdmin no-ops rather
	// than trying to seed (a half-set pair would be fatal, unrelated to the SUT).
	t.Setenv("OTHERIX_BOOTSTRAP_ADMIN_USERNAME", "")
	t.Setenv("OTHERIX_BOOTSTRAP_ADMIN_PASSWORD", "")

	// Hold the listen address so server.Run's ListenAndServe fails immediately
	// with EADDRINUSE. Kept bound for the whole test.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pre-bind listener: %v", err)
	}
	defer ln.Close()

	cfg := runServeTestConfig(ln.Addr().String(), clientURL)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// A NON-cancelled ctx: this is the whole point. On the graceful path SIGTERM
	// cancels ctx; here nothing does, so only the fix's cancelRun() can drive the
	// loops to Done.
	ctx := context.Background()

	baseline := runtime.NumGoroutine()

	errc := make(chan error, 1)
	go func() { errc <- runServe(ctx, cfg, st, svc, ca, log) }()

	select {
	case err := <-errc:
		if err == nil {
			t.Fatalf("runServe returned nil, want a server-start error")
		}
		if !strings.Contains(err.Error(), "server:") {
			t.Errorf("runServe error = %q, want it to contain %q", err.Error(), "server:")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runServe did not return within 5s: the background loops likely deadlocked the drain (no cancel on the non-cancelled error path)")
	}

	// The deferred drain runs before runServe returns, so once we have the error
	// the promote loop must already be gone. Poll to let transient goroutines
	// (the wrapper, runtime scheduler bookkeeping) exit before comparing.
	deadline := time.Now().Add(3 * time.Second)
	var got int
	for {
		got = runtime.NumGoroutine()
		if got <= baseline {
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("goroutine count = %d, want <= baseline %d after runServe returns; a background loop leaked past the failing server start", got, baseline)
}
