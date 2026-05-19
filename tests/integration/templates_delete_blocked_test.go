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

// TestTemplatesDelete_VerticalSliceBlockedByStorageImages exercises
// the extended template-delete refusal: a template
// with at least one storage_images projection cannot be deleted
// until the image is removed first. The test runs the full chain
// (real import → 409 on template delete → image delete → 204 on
// template delete) so the BlockingResourcesError envelope shape,
// the storage_images count, and the row-cleanup ordering all stay
// pinned.
func TestTemplatesDelete_VerticalSliceBlockedByStorageImages(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplate(t, ctx, v.store, v.adminID, "tpl-blocked", 0x66)
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

	// Attempt delete — must be blocked.
	resp := v.deleteTemplate(t, ctx, tpl.ID)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete template status = %d, body = %s, want 409", resp.StatusCode, body)
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
	if env.Error.Details.Kind != "template" {
		t.Errorf("error.details.kind = %q, want template", env.Error.Details.Kind)
	}
	if got := env.Error.Details.BlockingResources["storage_images"]; got != 1 {
		t.Errorf("details.blocking_resources.storage_images = %d, want 1", got)
	}

	// Cleanup: delete the image, then the template.
	delResp := v.deleteImage(t, ctx, v.pool.ID, imgRow.ID, "")
	delBody, _ := io.ReadAll(delResp.Body)
	_ = delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("delete image status = %d, body = %s, want 200", delResp.StatusCode, delBody)
	}
	resp2 := v.deleteTemplate(t, ctx, tpl.ID)
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("second delete template status = %d, want 204", resp2.StatusCode)
	}
}

// TestTemplatesDelete_VerticalSliceBlockedByVMs covers the
// active-VM extension к the existing template-delete refusal: a
// template underpinning at least one active VM cannot be deleted
// until the VMs are removed first. The handler runs CountActiveVMsForTemplate
// inside its InTx; this test exercises the full chain — real
// vm.create → 409 on template delete with details.blocking_resources.vms
// = 1 → vm delete → 204 on template delete.
func TestTemplatesDelete_VerticalSliceBlockedByVMs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplateForVM(t, ctx, v, v.adminID, "tpl-blocked-by-vm", 0x88, "private")

	vmName := "blocker-vm-" + uuid.NewString()[:8]
	createTaskID, _ := v.createVM(t, ctx, vmCreateBody{
		Name:     vmName,
		Template: tpl.Name,
		Pool:     v.pool.ID.String(),
		VCPUs:    1,
		MemoryMB: 512,
	}, "")
	v.awaitVMCreateEvent(t, 15*time.Second)
	createRow, _ := v.store.Queries().GetTask(ctx, createTaskID)
	vmID := extractVMIDFromTask(t, createRow)

	// Attempt template delete while VM is live → 409.
	resp := v.deleteTemplate(t, ctx, tpl.ID)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete template status = %d, body = %s, want 409", resp.StatusCode, body)
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				BlockingResources map[string]int `json:"blocking_resources"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Error.Code != "conflict" {
		// CountActiveVMsForTemplate-only branch keeps the generic
		// "conflict" code (not "resource_in_use" — that's the
		// stacked-storage-images branch).
		t.Errorf("error.code = %q, want conflict", env.Error.Code)
	}
	if got := env.Error.Details.BlockingResources["vms"]; got != 1 {
		t.Errorf("details.blocking_resources.vms = %d, want 1", got)
	}

	// Delete the VM (full chain through the agent), then re-attempt
	// template delete → 204.
	stageVMDeleteSuccess(v, vmName, 30*time.Millisecond)
	deleteStatus, _ := v.deleteVMRequest(t, ctx, vmID, "", "")
	if deleteStatus != http.StatusAccepted {
		t.Fatalf("vm delete enqueue status = %d, want 202", deleteStatus)
	}
	v.awaitVMDeleteEvent(t, 15*time.Second)

	resp2 := v.deleteTemplate(t, ctx, tpl.ID)
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("second delete template status = %d, want 204", resp2.StatusCode)
	}
}

// deleteTemplate / deletePool are small helpers used by this and the
// storage_pools delete-blocked test next door. Kept here to avoid
// bloating storage_image_vertical_setup_test.go with deletion helpers
// — only these two scenarios need them.
func (v *verticalSlice) deleteTemplate(t *testing.T, ctx context.Context, templateID uuid.UUID) *http.Response {
	t.Helper()
	row, err := v.store.Queries().GetTemplate(ctx, templateID)
	if err != nil {
		t.Fatalf("resolve template name for %s: %v", templateID, err)
	}
	url := v.cpServer.URL + "/v1/templates/" + row.Name
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		t.Fatalf("new template delete request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+v.adminToken)
	resp, err := v.cpServer.Client().Do(req)
	if err != nil {
		t.Fatalf("template delete: %v", err)
	}
	return resp
}

func (v *verticalSlice) deletePool(t *testing.T, ctx context.Context, poolID uuid.UUID) *http.Response {
	t.Helper()
	url := v.cpServer.URL + "/v1/storage-pools/" + poolID.String()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		t.Fatalf("new pool delete request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+v.adminToken)
	resp, err := v.cpServer.Client().Do(req)
	if err != nil {
		t.Fatalf("pool delete: %v", err)
	}
	return resp
}
