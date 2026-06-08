// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentmock

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
)

func TestPoolToAPI_CapacityZeroIsNil(t *testing.T) {
	now := time.Now().UTC()
	p := storagePool{
		StoragePool: StoragePool{
			ID:   uuid.New(),
			Name: "p",
			Type: "local_dir",
			Path: "/p",
		},
		reportedAt: now,
	}
	got := poolToAPI(p)
	if got.CapacityBytes != nil {
		t.Errorf("CapacityBytes = %v, want nil for zero capacity", got.CapacityBytes)
	}
	if got.AvailableBytes != nil {
		t.Errorf("AvailableBytes = %v, want nil for zero", got.AvailableBytes)
	}
	if !got.ReportedAt.Equal(now) {
		t.Errorf("ReportedAt = %v, want %v", got.ReportedAt, now)
	}
}

func TestPoolToAPI_CapacityNonZeroIsPointer(t *testing.T) {
	p := storagePool{
		StoragePool: StoragePool{
			ID:             uuid.New(),
			Name:           "p",
			Type:           "local_dir",
			Path:           "/p",
			CapacityBytes:  100,
			AvailableBytes: 50,
		},
		reportedAt: time.Now().UTC(),
	}
	got := poolToAPI(p)
	if got.CapacityBytes == nil || *got.CapacityBytes != 100 {
		t.Errorf("CapacityBytes = %v, want pointer to 100", got.CapacityBytes)
	}
	if got.AvailableBytes == nil || *got.AvailableBytes != 50 {
		t.Errorf("AvailableBytes = %v, want pointer to 50", got.AvailableBytes)
	}
}

func TestMigrationToAPI_ZeroPortsOmitted(t *testing.T) {
	got := migrationToAPI(MigrationCapability{
		Host:           "127.0.0.1",
		PortRangeStart: 49152,
		PortRangeEnd:   49251,
	})
	want := agentapi.MigrationCapability{
		Host:           "127.0.0.1",
		PortRangeStart: 49152,
		PortRangeEnd:   49251,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}
