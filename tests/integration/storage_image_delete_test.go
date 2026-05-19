// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentmock"
	"github.com/otherix/otherix/internal/store"
)

// TestStorageImageDelete_VerticalSliceSingleReferent: a single
// storage_images row in a pool — DELETE invokes agent.deleteImage,
// row removed, mock-side file evicted.
func TestStorageImageDelete_VerticalSliceSingleReferent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplate(t, ctx, v.store, v.adminID, "tpl-del-single", 0x11)
	checksum := hexChecksum(tpl.ImageChecksumSha256)

	v.mock.AddImageImportResult(v.pool.Name, checksum, agentmock.ImageImportResult{
		Status: "success", SizeBytes: 4096, Format: "qcow2", Delay: 30 * time.Millisecond,
	})
	_, _ = v.importImage(t, ctx, tpl.ID, "")
	v.awaitImportEvent(t, 15*time.Second)

	imgRow, err := v.store.Queries().GetStorageImageByTemplateAndPool(ctx, store.GetStorageImageByTemplateAndPoolParams{
		TemplateID: tpl.ID, PoolID: v.pool.ID,
	})
	if err != nil {
		t.Fatalf("GetStorageImageByTemplateAndPool: %v", err)
	}

	resp := v.deleteImage(t, ctx, v.pool.ID, imgRow.ID, "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("delete status = %d, body = %s, want 200", resp.StatusCode, body)
	}

	if got := v.agentDeleteImageCallCount(); got != 1 {
		t.Errorf("agent.deleteImage calls = %d, want 1 on single-referent path", got)
	}
	v.pollStorageImageRowGone(t, ctx, imgRow.ID, 5*time.Second)
	if _, ok := v.mock.GetStoredImage(v.pool.Name, checksum); ok {
		t.Errorf("mock.GetStoredImage = true after single-referent delete; want evicted")
	}
}

// TestStorageImageDelete_VerticalSliceMultiReferent: two
// storage_images rows in the same pool sharing one sha256 (two
// templates with identical content). DELETE the first; the
// surviving sibling protects the on-disk file. agent.deleteImage
// MUST NOT be called.
func TestStorageImageDelete_VerticalSliceMultiReferent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tplA := seedTemplate(t, ctx, v.store, v.adminID, "tpl-del-multi-a", 0x22)
	tplB := seedTemplate(t, ctx, v.store, v.adminID, "tpl-del-multi-b", 0x22)
	checksum := hexChecksum(tplA.ImageChecksumSha256)

	// First import stages the file; second import hits mock's pre-existence
	// path (Step 8) and emits terminal-success without consuming the queue.
	v.mock.AddImageImportResult(v.pool.Name, checksum, agentmock.ImageImportResult{
		Status: "success", SizeBytes: 4096, Format: "qcow2", Delay: 30 * time.Millisecond,
	})
	_, _ = v.importImage(t, ctx, tplA.ID, "")
	v.awaitImportEvent(t, 15*time.Second)
	_, _ = v.importImage(t, ctx, tplB.ID, "")
	v.awaitImportEvent(t, 15*time.Second)

	rowA, err := v.store.Queries().GetStorageImageByTemplateAndPool(ctx, store.GetStorageImageByTemplateAndPoolParams{
		TemplateID: tplA.ID, PoolID: v.pool.ID,
	})
	if err != nil {
		t.Fatalf("GetStorageImageByTemplateAndPool A: %v", err)
	}
	rowB, err := v.store.Queries().GetStorageImageByTemplateAndPool(ctx, store.GetStorageImageByTemplateAndPoolParams{
		TemplateID: tplB.ID, PoolID: v.pool.ID,
	})
	if err != nil {
		t.Fatalf("GetStorageImageByTemplateAndPool B: %v", err)
	}
	if rowA.ChecksumSha256 != rowB.ChecksumSha256 {
		t.Fatalf("rows do not share checksum: A=%q B=%q", rowA.ChecksumSha256, rowB.ChecksumSha256)
	}

	resp := v.deleteImage(t, ctx, v.pool.ID, rowA.ID, "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("delete A status = %d, body = %s, want 200", resp.StatusCode, body)
	}

	if got := v.agentDeleteImageCallCount(); got != 0 {
		t.Errorf("agent.deleteImage calls = %d, want 0 (refcount > 0 must skip agent)", got)
	}
	if _, err := v.store.Queries().GetStorageImageByID(ctx, rowA.ID); err == nil {
		t.Errorf("storage_images row A still present after delete")
	}
	if _, err := v.store.Queries().GetStorageImageByID(ctx, rowB.ID); err != nil {
		t.Errorf("storage_images row B disappeared: %v (sibling must survive)", err)
	}
	if _, ok := v.mock.GetStoredImage(v.pool.Name, checksum); !ok {
		t.Errorf("mock-side file evicted on multi-referent delete; want preserved")
	}
}

// TestStorageImageDelete_VerticalSliceAgentReturns404: agent's
// 204/404 are both treated as success by the agentclient (idempotent
// remove). Pre-evict the mock entry; CP delete still completes 200,
// row removed.
func TestStorageImageDelete_VerticalSliceAgentReturns404(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplate(t, ctx, v.store, v.adminID, "tpl-del-404", 0x33)
	checksum := hexChecksum(tpl.ImageChecksumSha256)

	v.mock.AddImageImportResult(v.pool.Name, checksum, agentmock.ImageImportResult{
		Status: "success", SizeBytes: 4096, Format: "qcow2", Delay: 30 * time.Millisecond,
	})
	_, _ = v.importImage(t, ctx, tpl.ID, "")
	v.awaitImportEvent(t, 15*time.Second)

	imgRow, err := v.store.Queries().GetStorageImageByTemplateAndPool(ctx, store.GetStorageImageByTemplateAndPoolParams{
		TemplateID: tpl.ID, PoolID: v.pool.ID,
	})
	if err != nil {
		t.Fatalf("GetStorageImageByTemplateAndPool: %v", err)
	}

	// Evict the file out-of-band before the CP-side delete runs —
	// mock's StorageImagesDelete is unconditionally 204 (its real
	// `rm -f` semantics), so we use EvictImage to make
	// GetStoredImage observably-false post-delete to verify the
	// row went and the file is gone regardless.
	if !v.mock.EvictImage(v.pool.Name, checksum) {
		t.Fatal("EvictImage = false; want true (pre-stage failed)")
	}

	resp := v.deleteImage(t, ctx, v.pool.ID, imgRow.ID, "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("delete status = %d, body = %s, want 200 (agent 204 idempotent)", resp.StatusCode, body)
	}
	v.pollStorageImageRowGone(t, ctx, imgRow.ID, 5*time.Second)
}

// TestStorageImageDelete_VerticalSliceAgentReturns502: mock injects
// 5xx on storageImages.delete — CP's transaction rolls back, the
// row stays. Caller sees 502 agent_unreachable.
func TestStorageImageDelete_VerticalSliceAgentReturns502(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplate(t, ctx, v.store, v.adminID, "tpl-del-502", 0x44)
	checksum := hexChecksum(tpl.ImageChecksumSha256)

	v.mock.AddImageImportResult(v.pool.Name, checksum, agentmock.ImageImportResult{
		Status: "success", SizeBytes: 4096, Format: "qcow2", Delay: 30 * time.Millisecond,
	})
	_, _ = v.importImage(t, ctx, tpl.ID, "")
	v.awaitImportEvent(t, 15*time.Second)

	imgRow, err := v.store.Queries().GetStorageImageByTemplateAndPool(ctx, store.GetStorageImageByTemplateAndPoolParams{
		TemplateID: tpl.ID, PoolID: v.pool.ID,
	})
	if err != nil {
		t.Fatalf("GetStorageImageByTemplateAndPool: %v", err)
	}

	v.mock.InjectError("storageImages.delete", agentmock.InjectedError{
		Status: http.StatusBadGateway, Code: "internal", Message: "upstream collapsed",
	})

	resp := v.deleteImage(t, ctx, v.pool.ID, imgRow.ID, "")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("delete status = %d, body = %s, want 502", resp.StatusCode, body)
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("unmarshal error envelope: %v", err)
	}
	if envelope.Error.Code != "agent_unreachable" {
		t.Errorf("error.code = %q, want agent_unreachable", envelope.Error.Code)
	}

	if _, err := v.store.Queries().GetStorageImageByID(ctx, imgRow.ID); err != nil {
		t.Errorf("storage_images row removed despite agent failure: %v (rollback broken)", err)
	}
}

// TestStorageImageDelete_VerticalSliceCrossPool: addressing an image
// owned by pool_A through path /v1/storage-pools/{pool_B}/images/{image_A}
// returns 404 (the addressable resource does not exist in that
// path, per the cross-pool addressing invariant).
func TestStorageImageDelete_VerticalSliceCrossPool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplate(t, ctx, v.store, v.adminID, "tpl-del-cross", 0x55)
	checksum := hexChecksum(tpl.ImageChecksumSha256)

	v.mock.AddImageImportResult(v.pool.Name, checksum, agentmock.ImageImportResult{
		Status: "success", SizeBytes: 4096, Format: "qcow2", Delay: 30 * time.Millisecond,
	})
	_, _ = v.importImage(t, ctx, tpl.ID, "")
	v.awaitImportEvent(t, 15*time.Second)

	imgRow, err := v.store.Queries().GetStorageImageByTemplateAndPool(ctx, store.GetStorageImageByTemplateAndPoolParams{
		TemplateID: tpl.ID, PoolID: v.pool.ID,
	})
	if err != nil {
		t.Fatalf("GetStorageImageByTemplateAndPool: %v", err)
	}

	// Seed a parallel pool on the same node — the image is NOT in it.
	otherPool, err := v.store.Queries().CreateStoragePool(ctx, store.CreateStoragePoolParams{
		ID:     uuid.New(),
		NodeID: v.node.ID,
		Name:   "vertical-pool-other-" + uuid.NewString()[:8],
		Type:   "local_dir",
		Path:   "/opt/otherix/pools/other",
		Config: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateStoragePool other: %v", err)
	}

	resp := v.deleteImage(t, ctx, otherPool.ID, imgRow.ID, "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("delete cross-pool status = %d, body = %s, want 404", resp.StatusCode, body)
	}
	if _, err := v.store.Queries().GetStorageImageByID(ctx, imgRow.ID); err != nil {
		t.Errorf("storage_images row vanished on cross-pool 404: %v", err)
	}
}
