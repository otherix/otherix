// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/agentmock"
	"github.com/otherix/otherix/internal/store"
)

// TestStorageImageImport_VerticalSliceCrashWindowB exercises the
// crash-recovery scenario in which an agent has already committed
// the image to the pool (e.g. a previous import finished on disk
// just before the agent crashed and CP did not project), and the
// operator re-issues the import after recovery.
//
// The mock simulates this state by pre-staging the image via
// AddImage. On the next import POST, mock-agent's pre-existence
// path (locked policy A) returns a terminal-success projection
// without consuming the
// AddImageImportResult queue — the queue stays empty for the
// duration of the test.
//
// CP-side, the worker observes the same terminal-success and
// projects storage_images normally. The end state is identical to
// a fresh import; the only observable difference is that the
// FIFO queue was not consumed, which the test asserts indirectly
// by verifying the storage_images row materialised even though
// no AddImageImportResult was staged.
func TestStorageImageImport_VerticalSliceCrashWindowB(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplate(t, ctx, v.store, v.adminID, "tpl-crash-b", 0x77)
	checksum := hexChecksum(tpl.ImageChecksumSha256)

	const wantSize int64 = 2 << 20
	if err := v.mock.AddImage(v.pool.Name, agentmock.CachedImage{
		ChecksumSHA256: checksum,
		Format:         "qcow2",
		SizeBytes:      wantSize,
		Path:           "/opt/otherix/pools/" + v.pool.ID.String() + "/" + checksum + ".qcow2",
	}); err != nil {
		t.Fatalf("mock.AddImage: %v", err)
	}

	// Intentionally do NOT call AddImageImportResult — the
	// pre-existence path must drive the outcome.

	taskID, _ := v.importImage(t, ctx, tpl.ID, "")
	v.awaitImportEvent(t, 15*time.Second)

	row, err := v.store.Queries().GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if row.Status != store.TaskStatusSuccess {
		t.Fatalf("task.Status = %q, want success (error=%s)", row.Status, string(row.Error))
	}

	imgRow, err := v.store.Queries().GetStorageImageByTemplateAndPool(ctx, store.GetStorageImageByTemplateAndPoolParams{
		TemplateID: tpl.ID,
		PoolID:     v.pool.ID,
	})
	if err != nil {
		t.Fatalf("GetStorageImageByTemplateAndPool: %v", err)
	}
	if imgRow.SizeBytes != wantSize {
		t.Errorf("storage_images.size_bytes = %d, want %d (pre-existing image's size_bytes must surface verbatim)", imgRow.SizeBytes, wantSize)
	}
}
