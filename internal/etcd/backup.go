// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// snapshotDialTimeout bounds the connect to the member client endpoint.
const snapshotDialTimeout = 5 * time.Second

// Snapshot files are named <prefix><sortable-utc-timestamp><suffix> so a lexical
// sort is chronological, which is how pruneSnapshots picks the oldest.
const (
	snapshotPrefix = "otherix-"
	snapshotSuffix = ".db"
)

// snapshotName builds the canonical snapshot filename for ts.
func snapshotName(ts time.Time) string {
	return snapshotPrefix + ts.UTC().Format("20060102T150405Z") + snapshotSuffix
}

// SnapshotSave streams a point-in-time snapshot of the member at clientURL to
// outPath and returns the number of bytes written. The snapshot is the member's
// own backend state (a bbolt db with an etcd integrity footer), restorable into
// a fresh data directory.
//
// It dials clientURL with a fresh network client rather than the co-located
// in-process client: the snapshot is a server-streaming RPC the in-process
// loopback adapter does not carry (it cancels the stream mid-transfer), so a
// real client endpoint is required. clientURL must be a single member - the
// snapshot is that member's state; in an HA cluster target a follower so the
// leader is not perturbed.
//
// The write is atomic: the stream lands in outPath+".tmp", is fsynced, then
// renamed over outPath, so a crash mid-write never leaves a truncated file at
// the canonical path.
//
// TODO(slice 9d): once peer/client mTLS lands, accept the client TLS material so
// snapshots work against a TLS-protected member endpoint.
func SnapshotSave(ctx context.Context, clientURL, outPath string) (int64, error) {
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{clientURL},
		DialTimeout: snapshotDialTimeout,
	})
	if err != nil {
		return 0, fmt.Errorf("snapshot client: %v", err)
	}
	defer func() { _ = cli.Close() }()

	rc, err := cli.Snapshot(ctx)
	if err != nil {
		return 0, fmt.Errorf("snapshot rpc: %v", err)
	}
	defer func() { _ = rc.Close() }()

	tmp := outPath + ".tmp"
	// 0600: the snapshot is a full copy of cluster state (password hashes,
	// token hashes, CA key) - owner-only, never group/world readable.
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // outPath is an operator-configured backup destination, not untrusted input
	if err != nil {
		return 0, fmt.Errorf("create snapshot file: %v", err)
	}

	n, copyErr := io.Copy(f, rc)
	if copyErr != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("write snapshot: %v", copyErr)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("sync snapshot: %v", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("close snapshot: %v", err)
	}
	if err := os.Rename(tmp, outPath); err != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("finalize snapshot: %v", err)
	}
	// fsync the parent dir so the rename's directory entry is crash-durable - a
	// backup that does not survive a crash right after the rename is worthless.
	if err := syncDir(filepath.Dir(outPath)); err != nil {
		return 0, fmt.Errorf("sync snapshot dir: %v", err)
	}
	return n, nil
}

// syncDir fsyncs a directory so a prior rename into it is crash-durable.
func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // operator-configured backup directory
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}

// pruneSnapshots keeps the newest `retention` snapshot files in dir (selected by
// the snapshot prefix/suffix, ordered by their sortable timestamped names) and
// removes the rest, returning how many it deleted. retention <= 0 disables
// pruning. Files that are not snapshots are never touched.
func pruneSnapshots(dir string, retention int) (int, error) {
	if retention <= 0 {
		return 0, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() && strings.HasPrefix(n, snapshotPrefix) && strings.HasSuffix(n, snapshotSuffix) {
			names = append(names, n)
		}
	}
	if len(names) <= retention {
		return 0, nil
	}
	sort.Strings(names) // chronological, oldest first
	removed := 0
	for _, n := range names[:len(names)-retention] {
		if err := os.Remove(filepath.Join(dir, n)); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// BackupFunc returns a periodic-worker function that snapshots the member at
// clientURL into dir (named by timestamp) and prunes to the newest `retention`
// snapshots. A snapshot failure fails the tick (the scheduler logs + retries
// next interval); a prune failure is logged but does not fail the tick, since
// the fresh snapshot already succeeded.
func BackupFunc(clientURL, dir string, retention int, log *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create backup dir: %v", err)
		}
		path := filepath.Join(dir, snapshotName(time.Now()))
		n, err := SnapshotSave(ctx, clientURL, path)
		if err != nil {
			return err
		}
		removed, perr := pruneSnapshots(dir, retention)
		if perr != nil {
			log.WarnContext(ctx, "etcd snapshot prune failed",
				slog.String("dir", dir), slog.String("error", perr.Error()))
		}
		log.InfoContext(ctx, "etcd snapshot saved",
			slog.String("path", path), slog.Int64("bytes", n), slog.Int("pruned", removed))
		return nil
	}
}
