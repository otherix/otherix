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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/etcd"
)

func TestSnapshotSave(t *testing.T) {
	clientURL := fmt.Sprintf("http://127.0.0.1:%d", freePort(t))
	cfg := &etcd.Config{
		Mode:         etcd.ModeSingle,
		Name:         "n1",
		DataDir:      filepath.Join(t.TempDir(), "member"),
		PeerURL:      fmt.Sprintf("http://127.0.0.1:%d", freePort(t)),
		ClientURL:    clientURL,
		ClusterToken: "otherix-test",
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	r, err := etcd.Start(ctx, cfg, log)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	cli := etcd.NewClient(r)
	t.Cleanup(func() {
		_ = cli.Close()
		r.Stop(10 * time.Second)
	})

	if err := cli.Put(ctx, etcd.Key("backup-test", "k"), []byte("v")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	out := filepath.Join(t.TempDir(), "snap.db")
	n, err := etcd.SnapshotSave(ctx, clientURL, out)
	if err != nil {
		t.Fatalf("SnapshotSave: %v", err)
	}
	if n <= 0 {
		t.Errorf("SnapshotSave returned %d bytes, want > 0", n)
	}

	fi, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat snapshot: %v", err)
	}
	if fi.Size() != n {
		t.Errorf("snapshot file size = %d, want %d (reported bytes)", fi.Size(), n)
	}
	// The .tmp staging file must not survive a successful save.
	if _, err := os.Stat(out + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("staging file %s.tmp still present after save", out)
	}
}
