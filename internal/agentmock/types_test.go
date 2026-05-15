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

func TestFirmwareToAPI_OmitsZeroes(t *testing.T) {
	got := firmwareToAPI(Firmware{
		Name:         "OVMF",
		Architecture: "amd64",
		Type:         "uefi",
		CodePath:     "/usr/share/OVMF/OVMF_CODE.fd",
	})
	if got.VarsTemplatePath != nil {
		t.Errorf("VarsTemplatePath = %v, want nil for empty input", got.VarsTemplatePath)
	}
	if got.SecureBoot != nil {
		t.Errorf("SecureBoot = %v, want nil for false input", got.SecureBoot)
	}
	if got.Name != "OVMF" {
		t.Errorf("Name = %q, want OVMF", got.Name)
	}
}

func TestFirmwareToAPI_PopulatesPointers(t *testing.T) {
	got := firmwareToAPI(Firmware{
		Name:             "OVMF_VARS",
		Architecture:     "arm64",
		Type:             "uefi",
		CodePath:         "/usr/share/AAVMF/AAVMF_CODE.fd",
		VarsTemplatePath: "/usr/share/AAVMF/AAVMF_VARS.fd",
		SecureBoot:       true,
	})
	if got.VarsTemplatePath == nil || *got.VarsTemplatePath != "/usr/share/AAVMF/AAVMF_VARS.fd" {
		t.Errorf("VarsTemplatePath = %v, want pointer to AAVMF path", got.VarsTemplatePath)
	}
	if got.SecureBoot == nil || !*got.SecureBoot {
		t.Errorf("SecureBoot = %v, want pointer to true", got.SecureBoot)
	}
}

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

func TestImageToAPI_ZeroLastUsedAtOmitted(t *testing.T) {
	got := imageToAPI(CachedImage{
		ChecksumSHA256: "abc",
		Format:         "qcow2",
		SizeBytes:      1024,
		Path:           "/p/x.qcow2",
	})
	if got.LastUsedAt != nil {
		t.Errorf("LastUsedAt = %v, want nil for zero time", got.LastUsedAt)
	}
	if got.SourceUrl != nil {
		t.Errorf("SourceUrl = %v, want nil for empty input", got.SourceUrl)
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
