// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
)

// sharedClient is a single embedded etcd member reused by every test in this
// binary, mirroring internal/etcdstore/main_test.go: one member plus a per-test
// keyspace wipe gives per-test isolation at a fraction of the per-test-member
// cost. Safe because tests run sequentially and none asserts absolute MVCC
// revisions.
var sharedClient *etcd.Client

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "otherix-migrations")
	if err != nil {
		fmt.Fprintf(os.Stderr, "mkdtemp: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	rt, err := etcd.Start(ctx, &etcd.Config{
		Mode:          etcd.ModeSingle,
		Name:          "migrations-test",
		DataDir:       filepath.Join(dir, "member"),
		PeerURL:       fmt.Sprintf("http://127.0.0.1:%d", freeTestPort()),
		ClientURL:     fmt.Sprintf("http://127.0.0.1:%d", freeTestPort()),
		ClusterToken:  "otherix-migrations-test",
		UnsafeNoFsync: true,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "etcd.Start: %v\n", err)
		os.Exit(1)
	}
	sharedClient = etcd.NewClient(rt)

	code := m.Run()

	_ = sharedClient.Close()
	rt.Stop(10 * time.Second)
	os.Exit(code)
}

// freshStore wipes the shared member's keyspace and returns a Store over it plus
// the raw client for direct seeding.
func freshStore(tb testing.TB) (*etcdstore.Store, *etcd.Client) {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := sharedClient.Raw().Delete(ctx, etcd.KeyPrefix, clientv3.WithPrefix()); err != nil {
		tb.Fatalf("wipe keyspace: %v", err)
	}
	return etcdstore.New(sharedClient), sharedClient
}

func freeTestPort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(fmt.Sprintf("freeTestPort: %v", err))
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
