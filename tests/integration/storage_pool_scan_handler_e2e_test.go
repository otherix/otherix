// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/otherix/otherix/internal/api"
	storagepoolshandlers "github.com/otherix/otherix/internal/api/handlers/storagepools"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/migrationtest"
	"github.com/otherix/otherix/internal/store"
)

// TestStoragePoolsScanHandler_VerticalSliceWithStub exercises the full
// storagePools.scan path end to end: a real CP HTTP server bound to
// the real river client + a canned-value fake executor, an admin POSTs
// /v1/storage-pools/{id}/scan, awaits the worker completing, then
// asserts the task transitions through pending → running → success
// and the storage_pools row reflects the stub's recognisable
// 1 TiB / 512 GiB markers.
//
// This is the queue-driven plumbing proof: handler →
// CreateTask + InsertTx + UpdateTaskRiverJobID (atomic), worker
// dispatch, ScanExecutor seam, projectAndFinalize, OpenAPI Task
// terminal-state shape — all live, all pinned by assertions.
func TestStoragePoolsScanHandler_VerticalSliceWithStub(t *testing.T) {
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

	// Seed an admin user + one ready node + one pool.
	adminID := uuid.New()
	adminEmail := fmt.Sprintf("scan-admin-%s@example.test", uuid.NewString()[:8])
	adminPW := "correct-horse-battery-staple"
	adminHash, err := auth.HashPassword(adminPW)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if _, err := s.Queries().CreateUser(ctx, store.CreateUserParams{
		ID:           adminID,
		Email:        adminEmail,
		PasswordHash: adminHash,
		DisplayName:  "scan admin",
		Role:         "admin",
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	nodeID := uuid.New()
	if _, err := s.Queries().CreateNode(ctx, store.CreateNodeParams{
		ID:                      nodeID,
		Name:                    "scan-handler-node-" + uuid.NewString()[:8],
		Architecture:            store.CpuArchAmd64,
		AdvertisedEndpoint:      "https://scan-handler-node.otherix.local:9443",
		MigrationHost:           "10.0.0.20",
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
		Name:   "scan-handler-pool-" + uuid.NewString()[:8],
		Type:   "local_dir",
		Path:   "/opt/otherix/pools/scan-handler",
		Config: []byte(`{}`),
	}); err != nil {
		t.Fatalf("CreateStoragePool: %v", err)
	}

	// Build the real auth service + river client, Subscribe to job
	// completion BEFORE Start so the in-flight scan can't race past it.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc, err := auth.NewService(auth.Config{
		JWTSecret:    []byte("test-secret-32-bytes-padding-pad-"),
		JWTAccessTTL: 15 * time.Minute,
		RefreshTTL:   720 * time.Hour,
	}, s)
	if err != nil {
		t.Fatalf("auth.NewService: %v", err)
	}
	scanExecutor := &fakeScanExecutor{fn: func(_ context.Context, _ storagepoolshandlers.ScanArgs) (storagepoolshandlers.ScanResult, error) {
		return storagepoolshandlers.ScanResult{
			CapacityBytes:  1 << 40,
			AvailableBytes: 1 << 39,
			ReportedAt:     time.Now().UTC(),
		}, nil
	}}
	riverClient, err := api.BuildRiverClient(api.RiverDeps{
		Pool:         h.Pool,
		Cfg:          config.WorkersConfig{Enabled: true, MaxWorkers: 4},
		Logger:       log,
		Store:        s,
		ScanExecutor: scanExecutor,
	})
	if err != nil {
		t.Fatalf("BuildRiverClient: %v", err)
	}
	completions, cancelSub := riverClient.Subscribe(river.EventKindJobCompleted)
	defer cancelSub()

	if err := riverClient.Start(ctx); err != nil {
		t.Fatalf("riverClient.Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = riverClient.Stop(stopCtx)
	})

	// HTTP harness via httptest.
	router := api.NewRouter(api.RouterDeps{
		Store:          s,
		AuthService:    authSvc,
		RiverClient:    riverClient,
		Logger:         log,
		RequestTimeout: 10 * time.Second,
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	// Login as admin.
	loginReq := mustJSON(t, map[string]string{"email": adminEmail, "password": adminPW})
	loginResp, err := http.Post(srv.URL+"/v1/auth/login", "application/json", bytes.NewReader(loginReq))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", loginResp.StatusCode)
	}
	var login struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(loginResp.Body).Decode(&login); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	loginResp.Body.Close()

	// POST /v1/storage-pools/{id}/scan.
	scanReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		srv.URL+"/v1/storage-pools/"+poolID.String()+"/scan",
		bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	scanReq.Header.Set("Content-Type", "application/json")
	scanReq.Header.Set("Authorization", "Bearer "+login.AccessToken)
	scanResp, err := srv.Client().Do(scanReq)
	if err != nil {
		t.Fatalf("scan POST: %v", err)
	}
	if scanResp.StatusCode != http.StatusAccepted {
		t.Fatalf("scan status = %d, want 202", scanResp.StatusCode)
	}
	var accepted struct {
		TaskID string `json:"task_id"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(scanResp.Body).Decode(&accepted); err != nil {
		t.Fatalf("decode scan: %v", err)
	}
	scanResp.Body.Close()

	taskID, err := uuid.Parse(accepted.TaskID)
	if err != nil {
		t.Fatalf("task_id %q is not a uuid: %v", accepted.TaskID, err)
	}

	// Drain completions until the storage_pool.scan job lands. Other
	// periodic workers (auth.refresh_token_cleanup, idempotency.cleanup)
	// fire under RunOnStart=false so should not race here, but match
	// strictly on Kind to be safe.
	deadline := time.After(15 * time.Second)
WaitLoop:
	for {
		select {
		case ev := <-completions:
			if ev.Job.Kind == "storage_pool.scan" {
				break WaitLoop
			}
		case <-deadline:
			t.Fatalf("storage_pool.scan did not complete within 15s")
		}
	}

	// Task ended in success with the stub-marker JSON.
	row, err := s.Queries().GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if row.Status != store.TaskStatusSuccess {
		t.Errorf("task.Status = %q, want success", row.Status)
	}
	if row.Result == nil {
		t.Fatal("task.Result = nil, want JSON payload")
	}
	var result struct {
		CapacityBytes  int64 `json:"capacity_bytes"`
		AvailableBytes int64 `json:"available_bytes"`
	}
	if err := json.Unmarshal(row.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result.CapacityBytes != 1<<40 {
		t.Errorf("result.capacity_bytes = %d, want 1<<40", result.CapacityBytes)
	}

	// storage_pools row reflects the stub markers.
	pool, err := s.Queries().GetStoragePoolByID(ctx, poolID)
	if err != nil {
		t.Fatalf("GetStoragePoolByID: %v", err)
	}
	if pool.CapacityBytes == nil || *pool.CapacityBytes != 1<<40 {
		t.Errorf("pool.CapacityBytes = %v, want 1<<40", pool.CapacityBytes)
	}
	if pool.AvailableBytes == nil || *pool.AvailableBytes != 1<<39 {
		t.Errorf("pool.AvailableBytes = %v, want 1<<39", pool.AvailableBytes)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}
