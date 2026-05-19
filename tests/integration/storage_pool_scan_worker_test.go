// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	storagepoolshandlers "github.com/otherix/otherix/internal/api/handlers/storagepools"
	"github.com/otherix/otherix/internal/migrationtest"
	"github.com/otherix/otherix/internal/store"
)

// scanFixture bundles the database state every scan-worker test needs:
// a real harness, a per-test-fresh user / node / pool / task. Each
// test calls scanFixtureSetup once and tears down via t.Cleanup.
type scanFixture struct {
	store  *store.Store
	pool   store.StoragePool
	node   store.Node
	taskID uuid.UUID
	logger *slog.Logger
}

func scanFixtureSetup(t *testing.T, ctx context.Context) *scanFixture {
	t.Helper()

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
		Email:        fmt.Sprintf("scan-fixture-%s@example.test", uuid.NewString()[:8]),
		PasswordHash: "x",
		DisplayName:  "scan fixture",
		Role:         "developer",
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	nodeID := uuid.New()
	node, err := s.Queries().CreateNode(ctx, store.CreateNodeParams{
		ID:                      nodeID,
		Name:                    "scan-fixture-node-" + uuid.NewString()[:8],
		Architecture:            store.CpuArchAmd64,
		AdvertisedEndpoint:      "https://scan-fixture-node.otherix.local:9443",
		MigrationHost:           "10.0.0.1",
		MigrationPortRangeStart: 49152,
		MigrationPortRangeEnd:   49251,
		Status:                  store.NodeStatusReady,
	})
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	poolID := uuid.New()
	pool, err := s.Queries().CreateStoragePool(ctx, store.CreateStoragePoolParams{
		ID:     poolID,
		NodeID: nodeID,
		Name:   "scan-fixture-pool-" + uuid.NewString()[:8],
		Type:   "local_dir",
		Path:   "/opt/otherix/pools/scan-fixture",
		Config: []byte(`{}`),
	})
	if err != nil {
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

	return &scanFixture{
		store:  s,
		pool:   pool,
		node:   node,
		taskID: taskID,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// fakeScanExecutor lets tests drive the executor seam directly: each
// Execute call runs the configured func, capturing the args observed
// for downstream assertions.
type fakeScanExecutor struct {
	fn         func(ctx context.Context, args storagepoolshandlers.ScanArgs) (storagepoolshandlers.ScanResult, error)
	calls      int
	lastCalled storagepoolshandlers.ScanArgs
}

func (f *fakeScanExecutor) Execute(ctx context.Context, args storagepoolshandlers.ScanArgs) (storagepoolshandlers.ScanResult, error) {
	f.calls++
	f.lastCalled = args
	if f.fn == nil {
		return storagepoolshandlers.ScanResult{}, errors.New("fakeScanExecutor: fn not set")
	}
	return f.fn(ctx, args)
}

// TestStoragePoolScanWorker_Success drives a happy lifecycle: stub
// executor returns canned values; worker projects them into
// storage_pools and finalizes the task as success.
func TestStoragePoolScanWorker_Success(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	f := scanFixtureSetup(t, ctx)

	worker := storagepoolshandlers.NewScanWorker(storagepoolshandlers.ScanDeps{
		Store: f.store,
		Executor: &fakeScanExecutor{fn: func(_ context.Context, _ storagepoolshandlers.ScanArgs) (storagepoolshandlers.ScanResult, error) {
			return storagepoolshandlers.ScanResult{CapacityBytes: 1 << 40, AvailableBytes: 1 << 39, ReportedAt: time.Now().UTC()}, nil
		}},
		Logger: f.logger,
	})
	if err := worker.Work(ctx, &river.Job[storagepoolshandlers.StoragePoolScanArgs]{
		Args: storagepoolshandlers.StoragePoolScanArgs{TaskID: f.taskID, PoolID: f.pool.ID},
	}); err != nil {
		t.Fatalf("Work: %v", err)
	}

	got, err := f.store.Queries().GetTask(ctx, f.taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != store.TaskStatusSuccess {
		t.Errorf("Status = %v, want success", got.Status)
	}
	if got.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", got.Attempts)
	}
	if got.StartedAt == nil || got.FinishedAt == nil {
		t.Errorf("StartedAt = %v, FinishedAt = %v, want both set",
			got.StartedAt, got.FinishedAt)
	}
	if got.Result == nil {
		t.Fatal("Result = nil, want JSON payload with stub values")
	}
	var result struct {
		CapacityBytes  int64     `json:"capacity_bytes"`
		AvailableBytes int64     `json:"available_bytes"`
		ReportedAt     time.Time `json:"reported_at"`
	}
	if err := json.Unmarshal(got.Result, &result); err != nil {
		t.Fatalf("unmarshal Result: %v", err)
	}
	if result.CapacityBytes != 1<<40 {
		t.Errorf("Result.CapacityBytes = %d, want 1<<40", result.CapacityBytes)
	}
	if result.AvailableBytes != 1<<39 {
		t.Errorf("Result.AvailableBytes = %d, want 1<<39", result.AvailableBytes)
	}
	if got.Error != nil {
		t.Errorf("Error = %s, want nil on success", string(got.Error))
	}

	// Projection assertion — storage_pools row reflects stub markers.
	pool, err := f.store.Queries().GetStoragePoolByID(ctx, f.pool.ID)
	if err != nil {
		t.Fatalf("GetStoragePoolByID: %v", err)
	}
	if pool.CapacityBytes == nil || *pool.CapacityBytes != 1<<40 {
		t.Errorf("pool.CapacityBytes = %v, want 1<<40", pool.CapacityBytes)
	}
	if pool.AvailableBytes == nil || *pool.AvailableBytes != 1<<39 {
		t.Errorf("pool.AvailableBytes = %v, want 1<<39", pool.AvailableBytes)
	}
	if pool.ReportedAt == nil {
		t.Error("pool.ReportedAt = nil, want set after upsert")
	}
}

// TestStoragePoolScanWorker_ExecutorError covers the failure branch:
// executor returns error → task transitions to failed with envelope,
// storage_pools left untouched.
func TestStoragePoolScanWorker_ExecutorError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	f := scanFixtureSetup(t, ctx)

	wantErr := errors.New("agent unreachable")
	fake := &fakeScanExecutor{
		fn: func(_ context.Context, _ storagepoolshandlers.ScanArgs) (storagepoolshandlers.ScanResult, error) {
			return storagepoolshandlers.ScanResult{}, wantErr
		},
	}
	worker := storagepoolshandlers.NewScanWorker(storagepoolshandlers.ScanDeps{
		Store:    f.store,
		Executor: fake,
		Logger:   f.logger,
	})

	err := worker.Work(ctx, &river.Job[storagepoolshandlers.StoragePoolScanArgs]{
		Args: storagepoolshandlers.StoragePoolScanArgs{TaskID: f.taskID, PoolID: f.pool.ID},
	})
	if err == nil {
		t.Fatal("Work returned nil, want non-nil so river retries")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("Work error = %q, want it to wrap %q", err, wantErr)
	}
	if fake.lastCalled.PoolID != f.pool.ID {
		t.Errorf("executor saw PoolID %v, want %v", fake.lastCalled.PoolID, f.pool.ID)
	}
	if fake.lastCalled.NodeID != f.node.ID {
		t.Errorf("executor saw NodeID %v, want %v", fake.lastCalled.NodeID, f.node.ID)
	}
	if fake.lastCalled.AdvertisedEndpoint != f.node.AdvertisedEndpoint {
		t.Errorf("executor saw AdvertisedEndpoint %q, want %q",
			fake.lastCalled.AdvertisedEndpoint, f.node.AdvertisedEndpoint)
	}

	got, err := f.store.Queries().GetTask(ctx, f.taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != store.TaskStatusFailed {
		t.Errorf("Status = %v, want failed", got.Status)
	}
	if got.Result != nil {
		t.Errorf("Result = %s, want nil on failure", string(got.Result))
	}
	if got.Error == nil {
		t.Fatal("Error = nil, want envelope")
	}
	var envelope struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(got.Error, &envelope); err != nil {
		t.Fatalf("unmarshal Error: %v", err)
	}
	if envelope.Code != "scan_failed" {
		t.Errorf("envelope.code = %q, want scan_failed", envelope.Code)
	}

	// storage_pools must be untouched on failure path.
	pool, err := f.store.Queries().GetStoragePoolByID(ctx, f.pool.ID)
	if err != nil {
		t.Fatalf("GetStoragePoolByID: %v", err)
	}
	if pool.CapacityBytes != nil || pool.AvailableBytes != nil || pool.ReportedAt != nil {
		t.Errorf("storage_pools touched on failure path: cap=%v avail=%v reported=%v",
			pool.CapacityBytes, pool.AvailableBytes, pool.ReportedAt)
	}
}

// TestStoragePoolScanWorker_PoolNotFound covers the pool-load failure
// branch: GetStoragePoolByID returns ErrNoRows after a soft-delete →
// task ends `failed` with code=pool_not_found, executor never invoked.
func TestStoragePoolScanWorker_PoolNotFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	f := scanFixtureSetup(t, ctx)
	if err := f.store.Queries().SoftDeleteStoragePool(ctx, f.pool.ID); err != nil {
		t.Fatalf("SoftDeleteStoragePool: %v", err)
	}

	fake := &fakeScanExecutor{
		fn: func(_ context.Context, _ storagepoolshandlers.ScanArgs) (storagepoolshandlers.ScanResult, error) {
			t.Fatal("executor invoked despite missing pool")
			return storagepoolshandlers.ScanResult{}, nil
		},
	}
	worker := storagepoolshandlers.NewScanWorker(storagepoolshandlers.ScanDeps{
		Store:    f.store,
		Executor: fake,
		Logger:   f.logger,
	})

	if err := worker.Work(ctx, &river.Job[storagepoolshandlers.StoragePoolScanArgs]{
		Args: storagepoolshandlers.StoragePoolScanArgs{TaskID: f.taskID, PoolID: f.pool.ID},
	}); err == nil {
		t.Fatal("Work returned nil, want non-nil")
	}
	if fake.calls != 0 {
		t.Errorf("executor called %d times, want 0", fake.calls)
	}

	got, err := f.store.Queries().GetTask(ctx, f.taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != store.TaskStatusFailed {
		t.Errorf("Status = %v, want failed", got.Status)
	}
	var envelope struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(got.Error, &envelope); err != nil {
		t.Fatalf("unmarshal Error: %v", err)
	}
	if envelope.Code != "pool_not_found" {
		t.Errorf("envelope.code = %q, want pool_not_found", envelope.Code)
	}
}

// TestStoragePoolScanWorker_NodeNotFound covers the node-load failure
// branch: GetNodeByID returns ErrNoRows after a node soft-delete →
// task ends `failed` with code=node_not_found.
//
// Pool soft-deletes don't cascade to vm_disks, but for nodes the FK
// from storage_pools is `on delete cascade` — a hard delete would
// cascade to the pool. Soft-delete just sets nodes.deleted_at; the
// pool row survives, GetNodeByID filters deleted_at and returns
// ErrNoRows, exactly the codepath we want.
func TestStoragePoolScanWorker_NodeNotFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	f := scanFixtureSetup(t, ctx)

	if _, err := f.store.Pool().Exec(ctx,
		`update nodes set deleted_at = now() where id = $1`, f.node.ID,
	); err != nil {
		t.Fatalf("soft-delete node: %v", err)
	}
	// Sanity: GetNodeByID now reports ErrNoRows.
	if _, err := f.store.Queries().GetNodeByID(ctx, f.node.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetNodeByID after soft-delete err = %v, want pgx.ErrNoRows", err)
	}

	fake := &fakeScanExecutor{
		fn: func(_ context.Context, _ storagepoolshandlers.ScanArgs) (storagepoolshandlers.ScanResult, error) {
			t.Fatal("executor invoked despite missing node")
			return storagepoolshandlers.ScanResult{}, nil
		},
	}
	worker := storagepoolshandlers.NewScanWorker(storagepoolshandlers.ScanDeps{
		Store:    f.store,
		Executor: fake,
		Logger:   f.logger,
	})

	if err := worker.Work(ctx, &river.Job[storagepoolshandlers.StoragePoolScanArgs]{
		Args: storagepoolshandlers.StoragePoolScanArgs{TaskID: f.taskID, PoolID: f.pool.ID},
	}); err == nil {
		t.Fatal("Work returned nil, want non-nil")
	}
	if fake.calls != 0 {
		t.Errorf("executor called %d times, want 0", fake.calls)
	}

	got, err := f.store.Queries().GetTask(ctx, f.taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != store.TaskStatusFailed {
		t.Errorf("Status = %v, want failed", got.Status)
	}
	var envelope struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(got.Error, &envelope); err != nil {
		t.Fatalf("unmarshal Error: %v", err)
	}
	if envelope.Code != "node_not_found" {
		t.Errorf("envelope.code = %q, want node_not_found", envelope.Code)
	}
}
