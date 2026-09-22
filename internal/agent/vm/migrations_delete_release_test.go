// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/qemu"
)

// TestDeleteReleasesIncomingServerBeforeRemovingTheDisk: a delete that reaches a
// completed cold migration's target before the reconciler released its server
// must stop the server BEFORE removing the disk, or the server keeps the
// unlinked file open and its space is never freed.
func TestDeleteReleasesIncomingServerBeforeRemovingTheDisk(t *testing.T) {
	m := newTestManager(t)
	m.migCreateDisk = func(_ context.Context, path string, _ int64) error {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return err
		}
		return os.WriteFile(path, []byte("qcow2"), 0o600)
	}
	m.migSpawnNBD = func(context.Context, []string) (*qemu.NBDServer, error) {
		return &qemu.NBDServer{Pid: 555}, nil
	}
	vmID := uuid.New()
	if _, err := m.StartIncoming(context.Background(), IncomingSpec{
		MigrationID: uuid.New(), VMUUID: vmID, VMName: "cold", VCPUs: 1, MemoryMib: 512,
		PoolName: m.defaultTestPool(), Architecture: "amd64", Mode: "offline",
		DiskSizeBytes: 1 << 30, SourceIdentity: "CN=node-src", BindHost: "10.0.0.2",
	}); err != nil {
		t.Fatalf("StartIncoming: %v", err)
	}
	v, err := m.snapshotVM(vmID)
	if err != nil {
		t.Fatalf("snapshotVM: %v", err)
	}
	diskExistedAtStop := false
	stopped := make(chan int, 1)
	m.migStopNBD = func(srv *qemu.NBDServer, _ time.Duration) error {
		_, statErr := os.Stat(v.DiskPath)
		diskExistedAtStop = statErr == nil
		stopped <- srv.Pid
		return nil
	}

	task, err := m.Delete(context.Background(), vmID)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	waitTaskTerminal(t, m, task.ID)

	select {
	case pid := <-stopped:
		if pid != 555 {
			t.Errorf("stopped pid %d, want 555", pid)
		}
	default:
		t.Fatalf("the incoming server was never stopped")
	}
	if !diskExistedAtStop {
		t.Errorf("disk already removed when the server was stopped, want the stop first")
	}
	if m.HasActiveMigration(vmID) {
		t.Errorf("HasActiveMigration = true after delete, want the record released")
	}
}
