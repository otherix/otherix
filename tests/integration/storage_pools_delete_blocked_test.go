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

// TestStoragePoolsDelete_VerticalSliceBlockedByStorageImages mirrors
// the templates-delete-blocked shape for the extended pool-delete
// refusal: a pool with at least one storage_images projection
// cannot be deleted until the image is removed first.
func TestStoragePoolsDelete_VerticalSliceBlockedByStorageImages(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplate(t, ctx, v.store, v.adminID, "tpl-pool-blocked", 0x88)
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

	resp := v.deletePool(t, ctx, v.pool.ID)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete pool status = %d, body = %s, want 409", resp.StatusCode, body)
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Kind              string         `json:"kind"`
				BlockingResources map[string]int `json:"blocking_resources"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Error.Code != "resource_in_use" {
		t.Errorf("error.code = %q, want resource_in_use", env.Error.Code)
	}
	if env.Error.Details.Kind != "pool" {
		t.Errorf("error.details.kind = %q, want pool", env.Error.Details.Kind)
	}
	if got := env.Error.Details.BlockingResources["storage_images"]; got != 1 {
		t.Errorf("details.blocking_resources.storage_images = %d, want 1", got)
	}

	delResp := v.deleteImage(t, ctx, v.pool.ID, imgRow.ID, "")
	_ = delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("delete image status = %d, want 200", delResp.StatusCode)
	}
	resp2 := v.deletePool(t, ctx, v.pool.ID)
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("second delete pool status = %d, want 204", resp2.StatusCode)
	}
}
