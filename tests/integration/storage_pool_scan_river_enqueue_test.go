// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package integration_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/otherix/otherix/internal/api"
	storagepoolshandlers "github.com/otherix/otherix/internal/api/handlers/storagepools"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/migrationtest"
	"github.com/otherix/otherix/internal/store"
)

// TestStoragePoolScanWorker_RiverEnqueue exercises the production
// wiring path — the worker registered by ScanWorkers, picked up by
// the real river client, processed end to end through the stub
// executor. Confirms the job kind flows correctly and the task ends
// in success after the queue dispatches it.
func TestStoragePoolScanWorker_RiverEnqueue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	h, err := migrationtest.Start(ctx)
	if err != nil {
		t.Fatalf("migrationtest.Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		h.Stop(stopCtx)
	})

	s := store.FromPool(h.Pool)

	userID := uuid.New()
	if _, err := s.Queries().CreateUser(ctx, store.CreateUserParams{
		ID:           userID,
		Email:        fmt.Sprintf("scan-river-%s@example.test", uuid.NewString()[:8]),
		PasswordHash: "x",
		DisplayName:  "scan river",
		Role:         "developer",
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	nodeID := uuid.New()
	if _, err := s.Queries().CreateNode(ctx, store.CreateNodeParams{
		ID:                      nodeID,
		Name:                    "scan-river-node-" + uuid.NewString()[:8],
		Architecture:            store.CpuArchAmd64,
		AdvertisedEndpoint:      "https://scan-river-node.otherix.local:9443",
		MigrationHost:           "10.0.0.2",
		MigrationPortRangeStart: 49152,
		MigrationPortRangeEnd:   49251,
		Status:                  store.NodeStatusReady,
	}); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	poolID := uuid.New()
	if _, err := s.Queries().CreateStoragePool(ctx, store.CreateStoragePoolParams{
		ID:     poolID,
		NodeID: nodeID,
		Name:   "scan-river-pool-" + uuid.NewString()[:8],
		Type:   "local_dir",
		Path:   "/opt/otherix/pools/scan-river",
		Config: []byte(`{}`),
	}); err != nil {
		t.Fatalf("CreateStoragePool: %v", err)
	}

	taskID := uuid.New()
	if _, err := s.Queries().CreateTask(ctx, store.CreateTaskParams{
		ID:           taskID,
		Type:         "storage_pool.scan",
		Status:       store.TaskStatusPending,
		ResourceType: "storage_pool",
		ResourceID:   &poolID,
		Args:         []byte(`{}`),
		MaxAttempts:  25,
		CreatedBy:    &userID,
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	scanExecutor := &fakeScanExecutor{fn: func(_ context.Context, _ storagepoolshandlers.ScanArgs) (storagepoolshandlers.ScanResult, error) {
		return storagepoolshandlers.ScanResult{
			CapacityBytes:  1 << 40,
			AvailableBytes: 1 << 39,
			ReportedAt:     time.Now().UTC(),
		}, nil
	}}
	client, err := api.BuildRiverClient(api.RiverDeps{
		Pool:         h.Pool,
		Cfg:          config.WorkersConfig{Enabled: true, MaxWorkers: 4},
		Logger:       log,
		Store:        s,
		ScanExecutor: scanExecutor,
	})
	if err != nil {
		t.Fatalf("BuildRiverClient: %v", err)
	}

	completions, cancelSub := client.Subscribe(river.EventKindJobCompleted)
	defer cancelSub()

	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = client.Stop(stopCtx)
	})

	if _, err := client.Insert(ctx,
		storagepoolshandlers.StoragePoolScanArgs{TaskID: taskID, PoolID: poolID},
		nil,
	); err != nil {
		t.Fatalf("Insert StoragePoolScanArgs: %v", err)
	}

	// Drain completions until our scan job reports — periodic auth /
	// idempotency cleanup workers may also fire and leak unrelated
	// EventKindJobCompleted events into this channel.
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-completions:
			if ev.Job.Kind == "storage_pool.scan" {
				goto Awaited
			}
		case <-deadline:
			t.Fatalf("storage_pool.scan did not complete within 10s")
		}
	}
Awaited:

	got, err := s.Queries().GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != store.TaskStatusSuccess {
		t.Errorf("Status = %v, want success", got.Status)
	}
	if got.Result == nil {
		t.Errorf("Result = nil, want stub-driven JSON payload")
	}

	pool, err := s.Queries().GetStoragePoolByID(ctx, poolID)
	if err != nil {
		t.Fatalf("GetStoragePoolByID: %v", err)
	}
	if pool.CapacityBytes == nil || *pool.CapacityBytes != 1<<40 {
		t.Errorf("pool.CapacityBytes = %v, want 1<<40 (stub marker)", pool.CapacityBytes)
	}
}
