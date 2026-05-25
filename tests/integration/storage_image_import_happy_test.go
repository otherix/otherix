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

	"github.com/otherix/otherix/internal/agentmock"
	"github.com/otherix/otherix/internal/store"
)

// TestStorageImageImport_VerticalSliceHappyPath drives the full
// import chain end-to-end: handler → real river → real worker
// (production agentImportExecutor) → functional mock-agent → poll →
// finalize → storage_images projection.
//
// The mock-agent is staged with a synthetic terminal-success outcome
// for the template's checksum; the test asserts the CP-side task
// transitions through pending → running → success, the
// storage_images row materialises with the expected
// (template_id, pool_id, checksum, size, format) projection, and
// the mock-agent's storedImages state reflects the imported file.
func TestStorageImageImport_VerticalSliceHappyPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplate(t, ctx, v.store, v.adminID, "tpl-happy", 0xab)
	checksum := hexChecksum(tpl.ImageChecksumSha256)

	const wantSize int64 = 1 << 20
	v.mock.AddImageImportResult(v.pool.Name, checksum, agentmock.ImageImportResult{
		Status:    "success",
		SizeBytes: wantSize,
		Format:    "qcow2",
		Delay:     30 * time.Millisecond,
	})

	taskID, _ := v.importImage(t, ctx, tpl.ID, "")
	v.awaitImportEvent(t, 15*time.Second)

	row, err := v.store.Queries().GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if row.Status != store.TaskStatusSuccess {
		t.Fatalf("task.Status = %q, want success (error=%s)", row.Status, string(row.Error))
	}
	if row.AgentTaskID == nil {
		t.Error("task.AgentTaskID is nil after success — resumption surface broken")
	}

	var result struct {
		ChecksumSHA256 string `json:"checksum_sha256"`
		SizeBytes      int64  `json:"size_bytes"`
		Format         string `json:"format"`
	}
	if err := json.Unmarshal(row.Result, &result); err != nil {
		t.Fatalf("unmarshal task.result: %v", err)
	}
	if result.ChecksumSHA256 != checksum {
		t.Errorf("result.checksum_sha256 = %q, want %q", result.ChecksumSHA256, checksum)
	}
	if result.SizeBytes != wantSize {
		t.Errorf("result.size_bytes = %d, want %d", result.SizeBytes, wantSize)
	}
	if result.Format != "qcow2" {
		t.Errorf("result.format = %q, want qcow2", result.Format)
	}

	imgRow, err := v.store.Queries().GetStorageImageByTemplateAndPool(ctx, store.GetStorageImageByTemplateAndPoolParams{
		TemplateID: tpl.ID,
		PoolID:     v.pool.ID,
	})
	if err != nil {
		t.Fatalf("GetStorageImageByTemplateAndPool: %v", err)
	}
	if imgRow.ChecksumSha256 != checksum {
		t.Errorf("storage_images.checksum = %q, want %q", imgRow.ChecksumSha256, checksum)
	}
	if imgRow.SizeBytes != wantSize {
		t.Errorf("storage_images.size_bytes = %d, want %d", imgRow.SizeBytes, wantSize)
	}

	cached, ok := v.mock.StoredImage(v.pool.Name, checksum)
	if !ok {
		t.Fatal("mock.StoredImage = false — terminal-success projection did not stage the file")
	}
	if cached.SizeBytes != wantSize {
		t.Errorf("mock.cached.SizeBytes = %d, want %d", cached.SizeBytes, wantSize)
	}

	// Subsequent GET /v1/storage-pools/{pool_id}/images/{image_id} surfaces the projection.
	getURL := v.cpServer.URL + "/v1/storage-pools/" + v.pool.ID.String() + "/images/" + imgRow.ID.String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, getURL, nil)
	if err != nil {
		t.Fatalf("new get request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+v.adminToken)
	resp, err := v.cpServer.Client().Do(req)
	if err != nil {
		t.Fatalf("get image: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get image status = %d, body = %s, want 200", resp.StatusCode, body)
	}
}
