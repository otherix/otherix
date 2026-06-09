// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentmock_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
	"github.com/otherix/otherix/internal/agentmock"
)

// startMock is a thin Start wrapper that returns a TLS-disabled mock
// — every functional test in this file calls the agent over HTTP.
// TLS handshake coverage lives in tls_test.go.
func startMock(t *testing.T) *agentmock.Mock {
	t.Helper()
	return agentmock.Start(t, agentmock.Options{Architecture: "amd64"})
}

// scanPool issues POST /v1/storage-pools/{poolName}/scan and returns
// the parsed AsyncTaskAccepted body. Tests fail-fast on any non-202
// or decode error.
func scanPool(t *testing.T, m *agentmock.Mock, poolName string) agentapi.AsyncTaskAccepted {
	t.Helper()
	url := m.URL() + "/v1/storage-pools/" + poolName + "/scan"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, http.NoBody)
	if err != nil {
		t.Fatalf("build scan request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("scan status = %d, want 202", resp.StatusCode)
	}
	var body agentapi.AsyncTaskAccepted
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode AsyncTaskAccepted: %v", err)
	}
	return body
}

// getTask issues GET /v1/tasks/{id} and returns (status, agentapi.Task).
// Status 404 is returned with a zero Task so callers can branch on
// the status without pre-checking the body.
func getTask(t *testing.T, m *agentmock.Mock, taskID uuid.UUID) (int, agentapi.Task) {
	t.Helper()
	url := m.URL() + "/v1/tasks/" + taskID.String()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("build tasks.get request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("tasks.get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, agentapi.Task{}
	}
	var body agentapi.Task
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode Task: %v", err)
	}
	return resp.StatusCode, body
}

// pollUntilTerminal polls tasks.get until status is terminal or the
// deadline fires. Returns the last-observed Task. Sleeps 5ms between
// polls — fast enough that a 50ms scan completes within a handful
// of iterations.
func pollUntilTerminal(t *testing.T, m *agentmock.Mock, taskID uuid.UUID, deadline time.Duration) agentapi.Task {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		status, body := getTask(t, m, taskID)
		if status != http.StatusOK {
			t.Fatalf("tasks.get status = %d, want 200 while polling", status)
		}
		switch string(body.Status) {
		case "success", "failed", "cancelled":
			return body
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("task %s did not reach terminal within %v", taskID, deadline)
	return agentapi.Task{}
}

func TestStoragePoolsScan_DefaultSuccess(t *testing.T) {
	t.Parallel()
	m := startMock(t)
	poolID := "test-pool"

	accepted := scanPool(t, m, poolID)
	if accepted.TaskID == uuid.Nil {
		t.Fatal("AsyncTaskAccepted.task_id is zero uuid")
	}
	if string(accepted.Status) != "pending" {
		t.Errorf("AsyncTaskAccepted.status = %q, want pending", accepted.Status)
	}
	want := "/v1/tasks/" + accepted.TaskID.String()
	if accepted.Links.Self != want {
		t.Errorf("AsyncTaskAccepted.links.self = %q, want %q", accepted.Links.Self, want)
	}

	final := pollUntilTerminal(t, m, accepted.TaskID, 500*time.Millisecond)
	if string(final.Status) != "success" {
		t.Errorf("final status = %q, want success", final.Status)
	}
	if final.Result == nil {
		t.Fatal("final Result is nil; want populated map for default success")
	}
	if got := (*final.Result)["capacity_bytes"]; got != float64(0) {
		t.Errorf("default capacity_bytes = %v, want 0", got)
	}
	if got := (*final.Result)["available_bytes"]; got != float64(0) {
		t.Errorf("default available_bytes = %v, want 0", got)
	}
	if final.StartedAt == nil {
		t.Errorf("StartedAt is nil; want populated on terminal")
	}
	if final.FinishedAt == nil {
		t.Errorf("FinishedAt is nil; want populated on terminal")
	}
}

func TestStoragePoolsScan_QueuedSuccess(t *testing.T) {
	t.Parallel()
	m := startMock(t)
	poolID := "test-pool"

	m.AddPoolScanResult(poolID, agentmock.PoolScanResult{
		Status:         "success",
		CapacityBytes:  1 << 40,
		AvailableBytes: 1 << 39,
		Delay:          50 * time.Millisecond,
	})

	accepted := scanPool(t, m, poolID)
	final := pollUntilTerminal(t, m, accepted.TaskID, 500*time.Millisecond)

	if string(final.Status) != "success" {
		t.Errorf("status = %q, want success", final.Status)
	}
	if final.Result == nil {
		t.Fatal("Result is nil")
	}
	if got := (*final.Result)["capacity_bytes"]; got != float64(1<<40) {
		t.Errorf("capacity_bytes = %v, want %d", got, int64(1<<40))
	}
	if got := (*final.Result)["available_bytes"]; got != float64(1<<39) {
		t.Errorf("available_bytes = %v, want %d", got, int64(1<<39))
	}
	if _, ok := (*final.Result)["reported_at"].(string); !ok {
		t.Errorf("reported_at missing or not a string in Result")
	}
}

func TestStoragePoolsScan_QueuedFailure(t *testing.T) {
	t.Parallel()
	m := startMock(t)
	poolID := "test-pool"

	m.AddPoolScanResult(poolID, agentmock.PoolScanResult{
		Status: "failed",
		Error: &agentmock.ErrorEnvelope{
			Code:    "internal",
			Message: "disk fault",
			Details: map[string]any{"retryable": false},
		},
		Delay: 50 * time.Millisecond,
	})

	accepted := scanPool(t, m, poolID)
	final := pollUntilTerminal(t, m, accepted.TaskID, 500*time.Millisecond)

	if string(final.Status) != "failed" {
		t.Errorf("status = %q, want failed", final.Status)
	}
	if final.Result != nil {
		t.Errorf("Result = %+v, want nil on failed", *final.Result)
	}
	if final.Error == nil {
		t.Fatal("Error envelope is nil; want populated")
	}
	if final.Error.Code == nil || string(*final.Error.Code) != "internal" {
		t.Errorf("Error.Code = %v, want internal", final.Error.Code)
	}
	if final.Error.Message == nil || *final.Error.Message != "disk fault" {
		t.Errorf("Error.Message = %v, want 'disk fault'", final.Error.Message)
	}
	if final.Error.Details == nil {
		t.Fatal("Error.Details is nil")
	}
	if got := (*final.Error.Details)["retryable"]; got != false {
		t.Errorf("Error.Details.retryable = %v, want false", got)
	}
}

func TestStoragePoolsScan_FIFOOrder(t *testing.T) {
	t.Parallel()
	m := startMock(t)
	poolID := "test-pool"

	for i, cap := range []int64{100, 200, 300} {
		m.AddPoolScanResult(poolID, agentmock.PoolScanResult{
			Status:        "success",
			CapacityBytes: cap,
			Delay:         20 * time.Millisecond,
		})
		_ = i
	}

	for _, want := range []int64{100, 200, 300} {
		accepted := scanPool(t, m, poolID)
		final := pollUntilTerminal(t, m, accepted.TaskID, 500*time.Millisecond)
		if got := (*final.Result)["capacity_bytes"]; got != float64(want) {
			t.Errorf("capacity_bytes = %v, want %d", got, want)
		}
	}

	// Queue exhausted — fourth scan falls back to default success
	// with zero capacity.
	accepted := scanPool(t, m, poolID)
	final := pollUntilTerminal(t, m, accepted.TaskID, 500*time.Millisecond)
	if got := (*final.Result)["capacity_bytes"]; got != float64(0) {
		t.Errorf("post-exhaustion default capacity_bytes = %v, want 0", got)
	}
}

func TestStoragePoolsScan_PerPoolQueue(t *testing.T) {
	t.Parallel()
	m := startMock(t)
	poolA := "pool-a"
	poolB := "pool-b"

	m.AddPoolScanResult(poolA, agentmock.PoolScanResult{
		Status: "success", CapacityBytes: 111, Delay: 20 * time.Millisecond,
	})
	m.AddPoolScanResult(poolB, agentmock.PoolScanResult{
		Status: "success", CapacityBytes: 222, Delay: 20 * time.Millisecond,
	})

	acceptedA := scanPool(t, m, poolA)
	finalA := pollUntilTerminal(t, m, acceptedA.TaskID, 500*time.Millisecond)
	if got := (*finalA.Result)["capacity_bytes"]; got != float64(111) {
		t.Errorf("pool A capacity_bytes = %v, want 111", got)
	}

	acceptedB := scanPool(t, m, poolB)
	finalB := pollUntilTerminal(t, m, acceptedB.TaskID, 500*time.Millisecond)
	if got := (*finalB.Result)["capacity_bytes"]; got != float64(222) {
		t.Errorf("pool B capacity_bytes = %v, want 222", got)
	}
}

func TestStoragePoolsScan_ResourceID(t *testing.T) {
	t.Parallel()
	m := startMock(t)
	poolID := "test-pool"
	poolUUID := uuid.New()
	m.AddStoragePool(agentmock.StoragePool{
		ID:   poolUUID,
		Name: poolID,
		Type: "local_dir",
		Path: "/var/lib/otherix/pools/test-pool",
	})

	m.AddPoolScanResult(poolID, agentmock.PoolScanResult{Delay: 20 * time.Millisecond})
	accepted := scanPool(t, m, poolID)
	final := pollUntilTerminal(t, m, accepted.TaskID, 500*time.Millisecond)

	if final.ResourceID == nil {
		t.Fatal("ResourceId is nil")
	}
	if *final.ResourceID != poolUUID.String() {
		t.Errorf("ResourceId = %q, want %q", *final.ResourceID, poolUUID.String())
	}
	if final.ResourceType != "storage_pool" {
		t.Errorf("ResourceType = %q, want storage_pool", final.ResourceType)
	}
	if final.Type != "storage_pool.scan" {
		t.Errorf("Type = %q, want storage_pool.scan", final.Type)
	}
}

func TestTasksGet_NotFound(t *testing.T) {
	t.Parallel()
	m := startMock(t)
	status, _ := getTask(t, m, uuid.New())
	if status != http.StatusNotFound {
		t.Errorf("tasks.get on unknown id = %d, want 404", status)
	}
}

func TestTasksGet_PendingRunningTerminalProgression(t *testing.T) {
	t.Parallel()
	m := startMock(t)
	poolID := "test-pool"

	const delay = 200 * time.Millisecond
	m.AddPoolScanResult(poolID, agentmock.PoolScanResult{
		Status:        "success",
		CapacityBytes: 42,
		Delay:         delay,
	})

	accepted := scanPool(t, m, poolID)

	// Immediate poll: pending. The mock just registered the task, so
	// elapsed ≈ 0 < delay/2 = 100ms.
	if _, body := getTask(t, m, accepted.TaskID); string(body.Status) != "pending" {
		t.Errorf("immediate status = %q, want pending", body.Status)
	}

	// Poll once we expect to be inside the running window.
	time.Sleep(delay/2 + 20*time.Millisecond)
	if _, body := getTask(t, m, accepted.TaskID); string(body.Status) != "running" {
		t.Errorf("mid-delay status = %q, want running", body.Status)
	}

	// Poll past the terminal boundary.
	time.Sleep(delay/2 + 30*time.Millisecond)
	_, body := getTask(t, m, accepted.TaskID)
	if string(body.Status) != "success" {
		t.Errorf("post-delay status = %q, want success", body.Status)
	}
}

func TestStoragePoolsScan_HonoursInjection(t *testing.T) {
	t.Parallel()
	m := startMock(t)
	poolID := "test-pool"
	m.InjectError("storagePools.scan", agentmock.InjectedError{
		Status: 503, Code: "internal", Message: "agent overloaded",
	})

	url := m.URL() + "/v1/storage-pools/" + poolID + "/scan"
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, url, http.NoBody)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (injection)", resp.StatusCode)
	}

	// No agent task should have been created; tasks.get on a freshly
	// minted uuid stays 404.
	if status, _ := getTask(t, m, uuid.New()); status != http.StatusNotFound {
		t.Errorf("tasks.get after blocked scan = %d, want 404", status)
	}
}

func TestTasksGet_HonoursInjection(t *testing.T) {
	t.Parallel()
	m := startMock(t)
	poolID := "test-pool"
	m.AddPoolScanResult(poolID, agentmock.PoolScanResult{Delay: 20 * time.Millisecond})
	accepted := scanPool(t, m, poolID)

	m.InjectError("tasks.get", agentmock.InjectedError{
		Status: 500, Code: "internal", Message: "synthetic",
	})
	status, _ := getTask(t, m, accepted.TaskID)
	if status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (injection)", status)
	}

	// Subsequent calls (single-shot injection consumed) revert to
	// the functional path.
	final := pollUntilTerminal(t, m, accepted.TaskID, 500*time.Millisecond)
	if string(final.Status) != "success" {
		t.Errorf("post-injection status = %q, want success", final.Status)
	}
}

// TestTasksGet_Concurrent stresses the lazy-projection path against
// the single mutex. With -race the Go runtime detects any
// mutex-skipped state read; without -race the test is still useful
// as a regression guard against accidental shared-state mutations
// in projectAgentTask (which must be a pure function over its
// snapshot input).
func TestTasksGet_Concurrent(t *testing.T) {
	t.Parallel()
	m := startMock(t)
	poolID := "test-pool"
	m.AddPoolScanResult(poolID, agentmock.PoolScanResult{
		Status: "success", CapacityBytes: 7, Delay: 100 * time.Millisecond,
	})
	accepted := scanPool(t, m, poolID)

	const goroutines = 16
	const polls = 20

	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make(chan error, goroutines*polls)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range polls {
				status, body := getTask(t, m, accepted.TaskID)
				if status != http.StatusOK {
					errs <- fmt.Errorf("status = %d", status)
					return
				}
				switch string(body.Status) {
				case "pending", "running", "success":
				default:
					errs <- fmt.Errorf("unexpected status %q", body.Status)
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestPoolScanResult_StatusDefault(t *testing.T) {
	t.Parallel()
	m := startMock(t)
	poolID := "test-pool"

	// Status omitted → defaults to success.
	m.AddPoolScanResult(poolID, agentmock.PoolScanResult{
		CapacityBytes: 1, Delay: 20 * time.Millisecond,
	})

	accepted := scanPool(t, m, poolID)
	final := pollUntilTerminal(t, m, accepted.TaskID, 500*time.Millisecond)
	if string(final.Status) != "success" {
		t.Errorf("status = %q, want success (empty Status defaults)", final.Status)
	}
}

func TestPoolScanResult_FailedWithoutErrorEnvelope(t *testing.T) {
	t.Parallel()
	m := startMock(t)
	poolID := "test-pool"
	// Failed without Error: agent task surfaces failed but no envelope.
	m.AddPoolScanResult(poolID, agentmock.PoolScanResult{
		Status: "failed", Delay: 20 * time.Millisecond,
	})

	accepted := scanPool(t, m, poolID)
	final := pollUntilTerminal(t, m, accepted.TaskID, 500*time.Millisecond)
	if string(final.Status) != "failed" {
		t.Errorf("status = %q, want failed", final.Status)
	}
	if final.Error != nil {
		t.Errorf("Error = %+v, want nil when PoolScanResult.Error is nil", final.Error)
	}
}

// TestStoragePoolsScan_AsyncTaskAcceptedShape complements the broader
// happy-path tests by asserting the strict shape of the 202 response
// body Content-Type / status code combination, which CP integration
// tests will rely on byte-exactly via agentclient.PostScan.
func TestStoragePoolsScan_AsyncTaskAcceptedShape(t *testing.T) {
	t.Parallel()
	m := startMock(t)
	poolID := "test-pool"

	url := m.URL() + "/v1/storage-pools/" + poolID + "/scan"
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, url, http.NoBody)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want 202", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json prefix", got)
	}
}
