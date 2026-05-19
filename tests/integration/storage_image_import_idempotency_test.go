// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentmock"
	"github.com/otherix/otherix/internal/store"
)

// TestStorageImageImport_VerticalSliceIdempotency verifies the
// CP-side Idempotency-Key middleware caches the 202 verbatim:
// a second POST with the same (Idempotency-Key,
// user_id, method, path, body-hash) replays the original task id
// without enqueueing a second river job.
//
// The test fires both POSTs synchronously back-to-back so the
// second request reads the cached response while the first job is
// still in-flight (middleware unit tests already cover the
// in-flight 409 vs cached-replay branching). After both
// requests we await the single completion event, assert exactly
// one storage_images row materialises, and assert the mock-agent
// observed exactly one storageImages.import call.
func TestStorageImageImport_VerticalSliceIdempotency(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplate(t, ctx, v.store, v.adminID, "tpl-idem", 0xcd)
	checksum := hexChecksum(tpl.ImageChecksumSha256)

	v.mock.AddImageImportResult(v.pool.Name, checksum, agentmock.ImageImportResult{
		Status:    "success",
		SizeBytes: 4096,
		Format:    "qcow2",
		Delay:     30 * time.Millisecond,
	})

	idemKey := "vertical-idem-" + uuid.NewString()

	taskA, _ := v.importImage(t, ctx, tpl.ID, idemKey)
	taskB, _ := v.importImage(t, ctx, tpl.ID, idemKey)

	if taskA != taskB {
		t.Fatalf("idempotency replay produced different task ids: A=%s B=%s", taskA, taskB)
	}

	v.awaitImportEvent(t, 15*time.Second)

	row, err := v.store.Queries().GetTask(ctx, taskA)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if row.Status != store.TaskStatusSuccess {
		t.Fatalf("task.Status = %q, want success (error=%s)", row.Status, string(row.Error))
	}

	page, err := v.store.Queries().ListStorageImagesByPool(ctx, store.ListStorageImagesByPoolParams{
		PoolID:     v.pool.ID,
		LimitCount: 100,
	})
	if err != nil {
		t.Fatalf("ListStorageImagesByPool: %v", err)
	}
	if len(page) != 1 {
		t.Errorf("storage_images rows = %d, want 1 (idempotency replay must not double-project)", len(page))
	}

	if got := v.agentImportCallCount(); got != 1 {
		t.Errorf("mock.storageImages.import calls = %d, want 1 (replay must not call agent twice)", got)
	}
}
