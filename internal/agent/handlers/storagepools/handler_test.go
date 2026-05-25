// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/vm"
	"github.com/otherix/otherix/internal/config"
)

// testPoolRoots remembers the filesystem root associated with each pool
// name minted by newTestManager. Image-surface tests retrieve the
// root via poolRootForTest to seed / inspect files. Keeps the existing
// 2-value newTestManager signature stable; nothing observable from
// production code.
var testPoolRoots sync.Map // map[string]string

// newTestManager spins up a vm.Manager backed by a temp pool. Mirrors
// the pattern in internal/agent/vm/manager_test.go. Returns the
// manager and the pool's name so the test can drive scan requests.
//
// The pool's filesystem root is cached in testPoolRoots so image
// tests can recover it via poolRootForTest.
func newTestManager(t *testing.T) (*vm.Manager, string) {
	t.Helper()
	tmp := t.TempDir()
	poolName := "default"
	poolRoot := filepath.Join(tmp, "pools", "default")
	cfg := &config.AgentConfig{
		StatePath: filepath.Join(tmp, "vms"),
		QEMU:      config.QEMUConfig{AArch64FirmwarePath: "/usr/share/AAVMF/AAVMF_CODE.fd"},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := vm.New(cfg, logger)
	if err != nil {
		t.Fatalf("vm.New: %v", err)
	}
	if err := m.AddPool(poolName, poolRoot); err != nil {
		t.Fatalf("vm.Manager.AddPool: %v", err)
	}
	testPoolRoots.Store(poolName, poolRoot)
	t.Cleanup(func() { testPoolRoots.Delete(poolName) })
	return m, poolName
}

// poolRootForTest returns the pool root cached by newTestManager.
func poolRootForTest(t *testing.T, _ *vm.Manager, poolName string) string {
	t.Helper()
	v, ok := testPoolRoots.Load(poolName)
	if !ok {
		t.Fatalf("pool root not registered for %s; use newTestManager", poolName)
	}
	root, _ := v.(string)
	return root
}

// drainAgentTask polls m.Task(id) until the task reaches a terminal
// status (success / failed) or budget expires. Mirrors
// waitForTaskTerminal in internal/agent/vm/scan_test.go; prevents
// t.TempDir auto-cleanup from racing with handler-spawned goroutines that
// outlive the synchronous 202 response (ImportImage writes to scratch
// dir; Scan only stats — drained for symmetry against future growth).
func drainAgentTask(t *testing.T, m *vm.Manager, id uuid.UUID, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		task := m.Task(id)
		if task == nil {
			t.Fatalf("task %s disappeared from store", id)
		}
		if task.Status == vm.TaskStatusSuccess || task.Status == vm.TaskStatusFailed {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("task %s did not reach terminal status within %s", id, budget)
}

// mountTestRouter wires the handler under /v1/storage-pools on a chi
// router shaped like the production agent router (path scope-only;
// middleware bypassed for unit isolation).
func mountTestRouter(h *Handler) chi.Router {
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		r.Route("/storage-pools", h.Mount)
	})
	return r
}

func TestScan_HappyPath(t *testing.T) {
	m, poolName := newTestManager(t)
	h := New(m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	router := mountTestRouter(h)

	req := httptest.NewRequest(http.MethodPost, "/v1/storage-pools/"+poolName+"/scan", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}

	var body asyncAccepted
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v; body = %s", err, rec.Body.String())
	}
	taskID, err := uuid.Parse(body.TaskID)
	if err != nil {
		t.Errorf("task_id = %q, want UUID", body.TaskID)
	}
	// Spec restricts AsyncTaskAccepted.status to [pending, running]. The
	// goroutine may finish before the handler returns, so the status
	// could be either when read out of the in-memory store. Either
	// value satisfies the contract.
	switch body.Status {
	case "pending", "running":
		// expected
	default:
		t.Errorf("status = %q, want pending or running", body.Status)
	}
	self, ok := body.Links["self"].(string)
	if !ok || self != "/v1/tasks/"+body.TaskID {
		t.Errorf("links.self = %v, want /v1/tasks/%s", body.Links["self"], body.TaskID)
	}
	drainAgentTask(t, m, taskID, 5*time.Second)
}

func TestScan_UnknownPool(t *testing.T) {
	m, _ := newTestManager(t)
	h := New(m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	router := mountTestRouter(h)

	// Random UUID — not in the manager's registry.
	req := httptest.NewRequest(http.MethodPost, "/v1/storage-pools/"+uuid.New().String()+"/scan", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v; body = %s", err, rec.Body.String())
	}
	if env.Error.Code != "not_found" {
		t.Errorf("error.code = %q, want %q", env.Error.Code, "not_found")
	}
}

// TestScan_NonUUIDPoolName confirms that any non-empty string is treated
// as a pool name and routed to the manager. Unknown names — including
// strings that don't look like UUIDs — return 404 not_found from the
// manager, not 400 validation_failed. Empty pool_name is the only path
// that yields 400, but it cannot be expressed in a chi URL.
func TestScan_NonUUIDPoolName(t *testing.T) {
	m, _ := newTestManager(t)
	h := New(m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	router := mountTestRouter(h)

	req := httptest.NewRequest(http.MethodPost, "/v1/storage-pools/not-a-uuid/scan", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v; body = %s", err, rec.Body.String())
	}
	if env.Error.Code != "not_found" {
		t.Errorf("error.code = %q, want %q", env.Error.Code, "not_found")
	}
}
