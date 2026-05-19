// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentmock"
	"github.com/otherix/otherix/internal/store"
)

// TestStorageImageImport_ComputeMode_BackPropagatesChecksum drives the
// compute-mode flow end-to-end:
//
//  1. CP-side template registered with NULL image_checksum_sha256 (no
//     upstream hash provided by the operator);
//  2. POST /v1/templates/{name}/images enqueues а storage_image.import
//     task; worker dispatches to the production executor;
//  3. Executor sends the agent request without expected_checksum_sha256
//     (omitempty on а *string field strips it от the wire body);
//  4. Mock-agent recognises compute mode и surfaces the queued
//     ComputedChecksumSHA256 in task.result.checksum_sha256;
//  5. Worker projects the result в storage_images AND back-propagates
//     the computed value onto the template row via
//     UpdateTemplateImageChecksumIfNull (idempotent IS NULL guard).
//
// The test asserts the template row transitions from NULL к the agent-
// computed value, locking the round-trip CP ↔ agent contract.
func TestStorageImageImport_ComputeMode_BackPropagatesChecksum(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())

	// Seed а compute-mode template — ImageChecksumSha256 nil →
	// NULL bytea в DB → executor sends agent request с omitted
	// expected_checksum_sha256.
	tpl, err := v.store.Queries().CreateTemplate(ctx, store.CreateTemplateParams{
		ID:                     uuid.New(),
		OwnerID:                v.adminID,
		Name:                   "tpl-compute-" + uuid.NewString()[:8],
		Description:            "compute-mode fixture",
		Architecture:           store.CpuArchAmd64,
		OsFamily:               store.OsFamilyLinux,
		OsVariant:              "ubuntu-24.04",
		ImageUrl:               "https://images.example.test/compute.qcow2",
		ImageChecksumSha256:    nil, // <-- compute mode
		ImageFormat:            store.ImageFormatQcow2,
		ImageSizeBytes:         nil,
		FirmwareType:           store.FirmwareTypeUefi,
		FirmwareID:             nil,
		DefaultCpuCores:        2,
		DefaultMemoryMib:       2048,
		DefaultDiskGib:         20,
		CloudInitSupported:     true,
		QemuGuestAgentExpected: false,
		Metadata:               []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateTemplate compute-mode fixture: %v", err)
	}

	// Stage the agent-computed checksum the mock will surface в
	// task.result. The mock-agent's compute-mode FIFO is keyed by the
	// empty string (matches how а real agent would respond when
	// expected_checksum_sha256 is absent).
	wantComputed := hex.EncodeToString(bytes.Repeat([]byte{0xc1}, 32))
	const wantSize int64 = 2 << 20
	v.mock.AddImageImportResult(v.pool.Name, "", agentmock.ImageImportResult{
		Status:                 "success",
		SizeBytes:              wantSize,
		Format:                 "qcow2",
		Delay:                  30 * time.Millisecond,
		ComputedChecksumSHA256: wantComputed,
	})

	taskID, _ := v.importImage(t, ctx, tpl.ID, "")
	v.awaitImportEvent(t, 15*time.Second)

	// Terminal status.
	row, err := v.store.Queries().GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if row.Status != store.TaskStatusSuccess {
		t.Fatalf("task.Status = %q, want success (error=%s)", row.Status, string(row.Error))
	}

	// Result carries the computed checksum.
	var result struct {
		ChecksumSHA256 string `json:"checksum_sha256"`
		SizeBytes      int64  `json:"size_bytes"`
		Format         string `json:"format"`
	}
	if err := json.Unmarshal(row.Result, &result); err != nil {
		t.Fatalf("unmarshal task.result: %v", err)
	}
	if result.ChecksumSHA256 != wantComputed {
		t.Errorf("task.result.checksum_sha256 = %q, want %q (agent-computed)",
			result.ChecksumSHA256, wantComputed)
	}

	// storage_images row projected с the computed checksum.
	imgRow, err := v.store.Queries().GetStorageImageByTemplateAndPool(ctx, store.GetStorageImageByTemplateAndPoolParams{
		TemplateID: tpl.ID,
		PoolID:     v.pool.ID,
	})
	if err != nil {
		t.Fatalf("GetStorageImageByTemplateAndPool: %v", err)
	}
	if imgRow.ChecksumSha256 != wantComputed {
		t.Errorf("storage_images.checksum = %q, want %q", imgRow.ChecksumSha256, wantComputed)
	}
	if imgRow.SizeBytes != wantSize {
		t.Errorf("storage_images.size_bytes = %d, want %d", imgRow.SizeBytes, wantSize)
	}

	// Back-propagation: template row now carries the computed bytes.
	freshTpl, err := v.store.Queries().GetTemplate(ctx, tpl.ID)
	if err != nil {
		t.Fatalf("GetTemplate after import: %v", err)
	}
	gotChecksum := hex.EncodeToString(freshTpl.ImageChecksumSha256)
	if gotChecksum != wantComputed {
		t.Errorf("template.image_checksum_sha256 = %q, want %q (back-propagated)",
			gotChecksum, wantComputed)
	}
}

// TestStorageImageImport_VerifyMode_DoesNotOverwriteChecksum confirms
// the IS NULL guard on UpdateTemplateImageChecksumIfNull: а template
// that was registered с а checksum (verify mode) does not have its
// column overwritten by а successful import, even though the worker
// runs the same back-propagation call site.
func TestStorageImageImport_VerifyMode_DoesNotOverwriteChecksum(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplate(t, ctx, v.store, v.adminID, "tpl-verify-preserve", 0xab)
	originalChecksum := hexChecksum(tpl.ImageChecksumSha256)

	// Stage а success outcome keyed by the verify-mode checksum.
	const wantSize int64 = 1 << 20
	v.mock.AddImageImportResult(v.pool.Name, originalChecksum, agentmock.ImageImportResult{
		Status:    "success",
		SizeBytes: wantSize,
		Format:    "qcow2",
		Delay:     30 * time.Millisecond,
	})

	_, _ = v.importImage(t, ctx, tpl.ID, "")
	v.awaitImportEvent(t, 15*time.Second)

	freshTpl, err := v.store.Queries().GetTemplate(ctx, tpl.ID)
	if err != nil {
		t.Fatalf("GetTemplate after import: %v", err)
	}
	if hexChecksum(freshTpl.ImageChecksumSha256) != originalChecksum {
		t.Errorf("template.image_checksum_sha256 mutated under verify mode: got %q, want %q",
			hexChecksum(freshTpl.ImageChecksumSha256), originalChecksum)
	}
}
