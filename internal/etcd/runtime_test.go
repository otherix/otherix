// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcd_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
)

// TestStartPersistsAcrossRestart drives the slice-1 DoD: a single-node embedded
// member starts, accepts a write through its client endpoint, survives a
// stop/start cycle, and replays the value on warm boot from the same data dir.
// Pure in-process (no Docker), but gated behind the integration tag because it
// binds loopback ports and writes a data dir.
func TestStartPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	var cfg *etcd.Config
	build := func() *etcd.Config {
		cfg = &etcd.Config{
			Mode:         etcd.ModeSingle,
			Name:         "n1",
			DataDir:      filepath.Join(dir, "member"),
			PeerURL:      fmt.Sprintf("http://127.0.0.1:%d", freePort(t)),
			ClientURL:    fmt.Sprintf("http://127.0.0.1:%d", freePort(t)),
			ClusterToken: "otherix-test",
		}
		return cfg
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Cold boot, write a value.
	r1 := startWithRetry(t, build)
	const key, want = "/otherix/test/hello", "world"
	if err := put(ctx, cfg.ClientURL, key, want); err != nil {
		r1.Stop(5 * time.Second)
		t.Fatalf("put(%q): %v", key, err)
	}
	r1.Stop(10 * time.Second)

	// Warm boot from the same data dir, verify the value survived. The member
	// recovers its membership from the WAL, so the fresh ports build re-rolls
	// per attempt are fine: only the data dir is identity.
	r2 := startWithRetry(t, build)
	defer r2.Stop(10 * time.Second)
	got, err := get(ctx, cfg.ClientURL, key)
	if err != nil {
		t.Fatalf("get(%q) after restart: %v", key, err)
	}
	if got != want {
		t.Errorf("get(%q) after restart = %q, want %q", key, got, want)
	}
}

// TestStartRejectsBadConfig confirms Start validates before touching the
// embedded server.
func TestStartRejectsBadConfig(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, err := etcd.Start(context.Background(), &etcd.Config{Mode: etcd.ModeSingle}, log)
	if err == nil {
		t.Fatal("Start(invalid config) = nil error, want validation failure")
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// maxStartAttempts bounds how many times startWithRetryFn re-rolls ports when
// an embedded member loses the freePort TOCTOU race.
const maxStartAttempts = 6

// startFn matches etcd.Start so tests can inject a stub.
type startFn func(context.Context, *etcd.Config, *slog.Logger) (*etcd.Runtime, error)

// isAddrInUse reports whether err is the OS "address already in use" bind
// failure - the signature of another process grabbing a port between
// freePort's probe and etcd's bind. Only this error class is retryable; any
// other startup failure is real and must surface immediately.
func isAddrInUse(err error) bool {
	return err != nil && strings.Contains(err.Error(), "address already in use")
}

// startWithRetryFn builds a config with build() and starts an embedded member,
// retrying up to maxStartAttempts times when the OS reports the chosen port
// was grabbed between freePort's probe and etcd's bind (a benign TOCTOU).
// build is called afresh each attempt so a new port set (and any
// initial-cluster string derived from it) is allocated per try. Non-bind
// errors fail immediately.
func startWithRetryFn(t *testing.T, start startFn, build func() *etcd.Config) *etcd.Runtime {
	t.Helper()
	var lastErr error
	for attempt := 1; attempt <= maxStartAttempts; attempt++ {
		cfg := build()
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		r, err := start(ctx, cfg, log)
		cancel()
		if err == nil {
			return r
		}
		if !isAddrInUse(err) {
			t.Fatalf("Start (attempt %d): %v", attempt, err)
		}
		lastErr = err
	}
	t.Fatalf("Start: port-bind conflict persisted across %d attempts: %v", maxStartAttempts, lastErr)
	return nil
}

// startWithRetry is startWithRetryFn wired to the real etcd.Start. Every
// test-side member start goes through it so a lost port race re-rolls fresh
// ports instead of flaking the suite.
func startWithRetry(t *testing.T, build func() *etcd.Config) *etcd.Runtime {
	t.Helper()
	return startWithRetryFn(t, etcd.Start, build)
}

// TestIsAddrInUse pins the retry decision: only the OS bind-conflict error is
// retryable; anything else (including nil) is not.
func TestIsAddrInUse(t *testing.T) {
	bindErr := fmt.Errorf("embed start: listen tcp 127.0.0.1:40889: bind: address already in use")
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"bind conflict", bindErr, true},
		{"wrapped bind conflict", fmt.Errorf("start n0: %v", bindErr), true},
		{"non-bind error", errors.New("embed startup timeout after 1m0s"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := isAddrInUse(tc.err); got != tc.want {
			t.Errorf("%s: isAddrInUse(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

// TestStartWithRetryFnReallocatesPortsPerAttempt drives the retry loop with a
// stub start that loses the bind race twice: build must run once per attempt
// (fresh ports each try) and the first successful runtime must be returned.
func TestStartWithRetryFnReallocatesPortsPerAttempt(t *testing.T) {
	builds, starts := 0, 0
	build := func() *etcd.Config {
		builds++
		return &etcd.Config{}
	}
	want := &etcd.Runtime{}
	stub := func(_ context.Context, _ *etcd.Config, _ *slog.Logger) (*etcd.Runtime, error) {
		starts++
		if starts <= 2 {
			return nil, fmt.Errorf("embed start: listen tcp 127.0.0.1:40889: bind: address already in use")
		}
		return want, nil
	}
	got := startWithRetryFn(t, stub, build)
	if got != want {
		t.Errorf("startWithRetryFn = %p, want the stub's runtime %p", got, want)
	}
	if builds != 3 {
		t.Errorf("build called %d times, want 3 (one fresh config per attempt)", builds)
	}
	if starts != 3 {
		t.Errorf("start called %d times, want 3", starts)
	}
}

func put(ctx context.Context, endpoint, key, value string) error {
	cli, err := clientv3.New(clientv3.Config{Endpoints: []string{endpoint}, DialTimeout: 5 * time.Second})
	if err != nil {
		return fmt.Errorf("clientv3.New: %v", err)
	}
	defer cli.Close()
	if _, err := cli.Put(ctx, key, value); err != nil {
		return fmt.Errorf("put: %v", err)
	}
	return nil
}

func get(ctx context.Context, endpoint, key string) (string, error) {
	cli, err := clientv3.New(clientv3.Config{Endpoints: []string{endpoint}, DialTimeout: 5 * time.Second})
	if err != nil {
		return "", fmt.Errorf("clientv3.New: %v", err)
	}
	defer cli.Close()
	resp, err := cli.Get(ctx, key)
	if err != nil {
		return "", fmt.Errorf("get: %v", err)
	}
	if len(resp.Kvs) == 0 {
		return "", fmt.Errorf("key %q not found", key)
	}
	return string(resp.Kvs[0].Value), nil
}
