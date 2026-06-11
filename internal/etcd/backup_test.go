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
	"strings"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/etcd"
)

// startMemberWithClientURL starts a single-node member, seeds one key, and
// returns its client URL (a network endpoint the etcd ops can dial). Shared by
// the backup and cluster integration tests.
func startMemberWithClientURL(t *testing.T) string {
	t.Helper()
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
	return clientURL
}

func TestSnapshotSave(t *testing.T) {
	clientURL := startMemberWithClientURL(t)

	out := filepath.Join(t.TempDir(), "snap.db")
	n, err := etcd.SnapshotSave(context.Background(), clientURL, out)
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
	// A snapshot is a full copy of cluster state (password hashes, token
	// hashes, CA key) - owner-only, never group/world readable.
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("snapshot file mode = %#o, want 0600", got)
	}
	if _, err := os.Stat(out + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("staging file %s.tmp still present after save", out)
	}
}

func TestBackupFuncCreatesSnapshot(t *testing.T) {
	clientURL := startMemberWithClientURL(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// BackupFunc must create the dir itself (it does not exist yet).
	backupDir := filepath.Join(t.TempDir(), "snaps")
	fn := etcd.BackupFunc(clientURL, backupDir, 7, log)
	if err := fn(context.Background()); err != nil {
		t.Fatalf("BackupFunc: %v", err)
	}

	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatalf("read backup dir: %v", err)
	}
	var snaps []os.DirEntry
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "otherix-") && strings.HasSuffix(e.Name(), ".db") {
			snaps = append(snaps, e)
		}
	}
	if len(snaps) != 1 {
		t.Fatalf("backup dir has %d snapshots, want 1", len(snaps))
	}
	if info, err := snaps[0].Info(); err != nil || info.Size() <= 0 {
		t.Errorf("snapshot %s size invalid: info=%v err=%v", snaps[0].Name(), info, err)
	}
}
