// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package integration_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/otherix/otherix/internal/agentmock"
	storagepoolshandlers "github.com/otherix/otherix/internal/api/handlers/storagepools"
	"github.com/otherix/otherix/internal/store"
)

// TestStorageImageImport_VerticalSliceResumption verifies the
// agent_task_id resumption surface introduced in Step 10. The test
// drives two import attempts against the same (template, pool):
//
//  1. The first attempt runs through the full happy path (CP POST
//     → river → worker → agent POST → poll → terminal-success →
//     storage_images projection). It establishes the mock-agent
//     task uuid and the CP-side tasks.agent_task_id linkage.
//
//  2. The CP-side row is deleted to simulate "projection lost
//     between agent commit and CP finalize" (same shape as the
//     Window B crash-recovery case, different cause). A second tasks row
//     is created with the *same* agent_task_id pre-populated and
//     status=running, simulating a CP-side restart mid-poll where
//     the worker's redelivery reads agent_task_id back from the row.
//
// Worker.Work is then invoked directly (bypasses river retry) with
// the fresh tasks row's id. The production agentImportExecutor
// observes args.AgentTaskID != nil and skips PostImageImport,
// going straight to PollTask. The mock's task projection still
// resolves to terminal-success on a second poll, so the worker
// reaches projectAndFinalize and re-creates the storage_images
// row through CreateStorageImage's ON CONFLICT DO UPDATE.
//
// The empirical assertion: agent saw exactly one
// storageImages.import call across both attempts. A regression in
// the resumption path (POST issued twice) would surface as a count
// of 2.
func TestStorageImageImport_VerticalSliceResumption(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplate(t, ctx, v.store, v.adminID, "tpl-resume", 0xee)
	checksum := hexChecksum(tpl.ImageChecksumSha256)

	v.mock.AddImageImportResult(v.pool.Name, checksum, agentmock.ImageImportResult{
		Status:    "success",
		SizeBytes: 1 << 20,
		Format:    "qcow2",
		Delay:     30 * time.Millisecond,
	})

	// First attempt - real import end-to-end.
	firstTaskID, _ := v.importImage(t, ctx, tpl.ID, "")
	v.awaitImportEvent(t, 15*time.Second)

	firstRow, err := v.store.Queries().GetTask(ctx, firstTaskID)
	if err != nil {
		t.Fatalf("GetTask first: %v", err)
	}
	if firstRow.Status != store.TaskStatusSuccess {
		t.Fatalf("first task.Status = %q, want success", firstRow.Status)
	}
	if firstRow.AgentTaskID == nil {
		t.Fatal("first task.AgentTaskID is nil — resumption seam not exercised")
	}
	agentTaskID := *firstRow.AgentTaskID

	if got := v.agentImportCallCount(); got != 1 {
		t.Fatalf("after first attempt, agent.import calls = %d, want 1", got)
	}

	firstImg, err := v.store.Queries().GetStorageImageByTemplateAndPool(ctx, store.GetStorageImageByTemplateAndPoolParams{
		TemplateID: tpl.ID,
		PoolID:     v.pool.ID,
	})
	if err != nil {
		t.Fatalf("GetStorageImageByTemplateAndPool: %v", err)
	}
	if err := v.store.Queries().DeleteStorageImage(ctx, firstImg.ID); err != nil {
		t.Fatalf("DeleteStorageImage: %v", err)
	}

	// Second attempt - fresh tasks row carrying the existing agent_task_id.
	freshTaskID := uuid.New()
	templateID := tpl.ID
	creatorID := v.adminID
	args, err := json.Marshal(map[string]any{
		"template_id": tpl.ID.String(),
		"pool_id":     v.pool.ID.String(),
	})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	if _, err := v.store.Queries().CreateTask(ctx, store.CreateTaskParams{
		ID:           freshTaskID,
		Type:         "storage_image.import",
		Status:       store.TaskStatusPending,
		ResourceType: "template",
		ResourceID:   &templateID,
		Args:         args,
		MaxAttempts:  25,
		CreatedBy:    &creatorID,
	}); err != nil {
		t.Fatalf("CreateTask fresh: %v", err)
	}
	if err := v.store.Queries().UpdateTaskAgentTaskID(ctx, store.UpdateTaskAgentTaskIDParams{
		ID:          freshTaskID,
		AgentTaskID: &agentTaskID,
	}); err != nil {
		t.Fatalf("UpdateTaskAgentTaskID: %v", err)
	}

	// Drive the worker directly — bypasses river so the test does
	// not have to mutate river_job rows.
	deps := storagepoolshandlers.ImportDeps{
		Store:    v.store,
		Executor: storagepoolshandlers.NewAgentImportExecutor(v.agentClient),
		Logger:   v.logger,
	}
	worker := storagepoolshandlers.NewImportWorker(deps)
	job := &river.Job[storagepoolshandlers.StorageImageImportArgs]{
		Args: storagepoolshandlers.StorageImageImportArgs{
			TaskID:     freshTaskID,
			TemplateID: tpl.ID,
			PoolID:     v.pool.ID,
		},
	}
	if err := worker.Work(ctx, job); err != nil {
		t.Fatalf("worker.Work resumption: %v", err)
	}

	freshRow, err := v.store.Queries().GetTask(ctx, freshTaskID)
	if err != nil {
		t.Fatalf("GetTask fresh: %v", err)
	}
	if freshRow.Status != store.TaskStatusSuccess {
		t.Fatalf("fresh task.Status = %q, want success", freshRow.Status)
	}

	if _, err := v.store.Queries().GetStorageImageByTemplateAndPool(ctx, store.GetStorageImageByTemplateAndPoolParams{
		TemplateID: tpl.ID,
		PoolID:     v.pool.ID,
	}); err != nil {
		t.Fatalf("storage_images row not re-created after resumption: %v", err)
	}

	if got := v.agentImportCallCount(); got != 1 {
		t.Errorf("after resumption, agent.import calls = %d, want 1 (resumption must skip POST)", got)
	}
}
