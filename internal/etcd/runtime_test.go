// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcd_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
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
	cfg := &etcd.Config{
		Mode:         etcd.ModeSingle,
		Name:         "n1",
		DataDir:      filepath.Join(dir, "member"),
		PeerURL:      fmt.Sprintf("http://127.0.0.1:%d", freePort(t)),
		ClientURL:    fmt.Sprintf("http://127.0.0.1:%d", freePort(t)),
		ClusterToken: "otherix-test",
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Cold boot, write a value.
	r1, err := etcd.Start(ctx, cfg, log)
	if err != nil {
		t.Fatalf("Start cold boot: %v", err)
	}
	const key, want = "/otherix/test/hello", "world"
	if err := put(ctx, cfg.ClientURL, key, want); err != nil {
		r1.Stop(5 * time.Second)
		t.Fatalf("put(%q): %v", key, err)
	}
	r1.Stop(10 * time.Second)

	// Warm boot from the same data dir, verify the value survived.
	r2, err := etcd.Start(ctx, cfg, log)
	if err != nil {
		t.Fatalf("Start warm boot: %v", err)
	}
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
