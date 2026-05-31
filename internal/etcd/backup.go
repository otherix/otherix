// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcd

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// snapshotDialTimeout bounds the connect to the member client endpoint.
const snapshotDialTimeout = 5 * time.Second

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
	f, err := os.Create(tmp) //nolint:gosec // outPath is an operator-configured backup destination, not untrusted input
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
	return n, nil
}
