// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore

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
)

// sharedClient is a single embedded etcd member reused by every test in this
// binary. Spinning up a fresh member per test (the old per-test startStore) cost
// ~1s each and dominated `make test-etcd`; one shared member plus a per-test
// keyspace wipe gives the same isolation at a fraction of the wall-clock. This
// mirrors tests/apie2e/harness_test.go. Safe because no test asserts absolute
// MVCC revisions (the only state a keyspace wipe cannot reset) and no test runs
// in parallel.
var sharedClient *etcd.Client

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "otherix-etcdstore")
	if err != nil {
		fmt.Fprintf(os.Stderr, "mkdtemp: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	rt, err := etcd.Start(ctx, &etcd.Config{
		Mode:          etcd.ModeSingle,
		Name:          "etcdstore-test",
		DataDir:       filepath.Join(dir, "member"),
		PeerURL:       fmt.Sprintf("http://127.0.0.1:%d", freeTestPort()),
		ClientURL:     fmt.Sprintf("http://127.0.0.1:%d", freeTestPort()),
		ClusterToken:  "otherix-etcdstore-test",
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

// FreshStore wipes the shared member's keyspace and returns a Store over it,
// plus the raw client for tests that seed index keys directly. Every test thus
// starts from an empty keyspace, so the shared member is as isolated as a
// per-test member would be. Exported so the external etcdstore_test package can
// reach it; tests run sequentially, so the wipe is race-free.
func FreshStore(tb testing.TB) (*Store, *etcd.Client) {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := sharedClient.Raw().Delete(ctx, etcd.KeyPrefix, clientv3.WithPrefix()); err != nil {
		tb.Fatalf("wipe keyspace: %v", err)
	}
	return New(sharedClient), sharedClient
}

func freeTestPort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(fmt.Sprintf("freeTestPort: %v", err))
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
