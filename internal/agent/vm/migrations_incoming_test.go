// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestStartIncomingReservesPortAndReturnsEndpoint(t *testing.T) {
	m := newTestManager(t)

	var createdDisk string
	var nbdArgs []string
	m.migCreateDisk = func(ctx context.Context, path string, virtualBytes int64) error {
		createdDisk = path
		return nil
	}
	m.migSpawnNBD = func(ctx context.Context, args []string) (int, error) {
		nbdArgs = args
		return 4321, nil
	}

	migID := uuid.New()
	res, err := m.StartIncoming(context.Background(), IncomingSpec{
		MigrationID:    migID,
		VMUUID:         uuid.New(),
		VMName:         "demo",
		VCPUs:          1,
		MemoryMB:       512,
		PoolName:       m.defaultTestPool(),
		Architecture:   "amd64",
		Mode:           "offline",
		ExpectedSize:   1 << 30,
		SourceIdentity: "CN=node-src",
		BindHost:       "10.0.0.2",
	})
	if err != nil {
		t.Fatalf("StartIncoming() error = %v", err)
	}
	if res.ListenEndpoint == "" || res.AuthToken == "" {
		t.Errorf("StartIncoming result endpoint/token empty: %+v", res)
	}
	if createdDisk == "" {
		t.Errorf("destination disk not created")
	}
	if !argsContain(nbdArgs, "authz-simple,id=migauthz,identity=CN=node-src") {
		t.Errorf("qemu-nbd args missing source authz pin: %v", nbdArgs)
	}
	rec, ok := m.Migrations().Get(migID)
	if !ok || rec.Role != "target" || rec.Phase != "setup" {
		t.Errorf("record = %+v, ok=%v", rec, ok)
	}
}

// TestStartIncomingSizesDestinationToMax pins the destination-disk sizing
// rule: the created qcow2 must be max(DiskSizeBytes, ExpectedSize). The disk
// must never be smaller than the source virtual size (DiskSizeBytes), or the
// source-side qemu-img convert push would fail; ExpectedSize is only a
// progress estimate and must not shrink the destination.
func TestStartIncomingSizesDestinationToMax(t *testing.T) {
	tests := []struct {
		name          string
		diskSizeBytes int64
		expectedSize  int64
		wantVirtual   int64
	}{
		{name: "disk size dominates", diskSizeBytes: 5 << 30, expectedSize: 1 << 30, wantVirtual: 5 << 30},
		{name: "expected size dominates", diskSizeBytes: 2 << 30, expectedSize: 8 << 30, wantVirtual: 8 << 30},
		{name: "disk size only", diskSizeBytes: 3 << 30, expectedSize: 0, wantVirtual: 3 << 30},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestManager(t)
			var gotVirtual int64
			m.migCreateDisk = func(_ context.Context, _ string, virtualBytes int64) error {
				gotVirtual = virtualBytes
				return nil
			}
			m.migSpawnNBD = func(_ context.Context, _ []string) (int, error) { return 1, nil }

			if _, err := m.StartIncoming(context.Background(), IncomingSpec{
				MigrationID:    uuid.New(),
				VMUUID:         uuid.New(),
				VMName:         "demo",
				VCPUs:          1,
				MemoryMB:       512,
				PoolName:       m.defaultTestPool(),
				Architecture:   "amd64",
				Mode:           "offline",
				DiskSizeBytes:  tc.diskSizeBytes,
				ExpectedSize:   tc.expectedSize,
				SourceIdentity: "CN=node-src",
				BindHost:       "10.0.0.2",
			}); err != nil {
				t.Fatalf("StartIncoming() error = %v", err)
			}
			if gotVirtual != tc.wantVirtual {
				t.Errorf("destination virtual size = %d, want %d", gotVirtual, tc.wantVirtual)
			}
		})
	}
}

func argsContain(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
