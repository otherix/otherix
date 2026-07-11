// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/console"
	"github.com/otherix/otherix/internal/agent/netfabric"
	"github.com/otherix/otherix/internal/agent/vm"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/config"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testImageURL is the HTTPS image source every create request in this
// file carries. The stub manager never actually fetches it (the async
// ensure fails on the unreachable host); the tests only assert API-edge
// behaviour up to the 202 hand-off.
const testImageURL = "https://example.test/ubuntu.img"

// newCreateHarness builds a Handler over a real manager and a spy
// fabric. When withPool is true a pool is materialised so a request that
// clears NIC validation reaches 202 (the async ensure then fails on the
// unreachable image host - fine; the tests assert the 202 hand-off).
func newCreateHarness(t *testing.T, fake *netfabric.FakeFabric, withPool bool) *Handler {
	t.Helper()
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	poolRoot := filepath.Join(tmp, "pool")
	const poolName = "default"

	cfg := &config.AgentConfig{StatePath: stateDir}
	m, err := vm.New(cfg, fake, discardLogger())
	if err != nil {
		t.Fatalf("vm.New: %v", err)
	}
	if withPool {
		if err := m.AddPool(poolName, poolRoot); err != nil {
			t.Fatalf("AddPool: %v", err)
		}
	}
	return New(m, console.NewTokenStore(), discardLogger(), "127.0.0.1", nil)
}

func TestCreate_NICEdgeValidation(t *testing.T) {
	const goodMAC = "52:54:00:12:34:56"
	validNIC := func() map[string]any {
		return map[string]any{
			"id":           uuid.NewString(),
			"bridge":       "br0",
			"mac":          goodMAC,
			"model":        "virtio",
			"mtu":          1500,
			"device_order": 0,
		}
	}

	cases := []struct {
		name string
		nics []map[string]any
	}{
		{
			name: "bad mac",
			nics: []map[string]any{func() map[string]any { n := validNIC(); n["mac"] = "not-a-mac"; return n }()},
		},
		{
			name: "bad model",
			nics: []map[string]any{func() map[string]any { n := validNIC(); n["model"] = "bogus"; return n }()},
		},
		{
			name: "empty bridge",
			nics: []map[string]any{func() map[string]any { n := validNIC(); n["bridge"] = ""; return n }()},
		},
		{
			name: "device_order out of range",
			nics: []map[string]any{func() map[string]any { n := validNIC(); n["device_order"] = 16; return n }()},
		},
		{
			name: "device_order negative",
			nics: []map[string]any{func() map[string]any { n := validNIC(); n["device_order"] = -1; return n }()},
		},
		{
			name: "duplicate device_order",
			nics: func() []map[string]any {
				a := validNIC()
				b := validNIC()
				b["mac"] = "52:54:00:12:34:57"
				a["device_order"] = 0
				b["device_order"] = 0
				return []map[string]any{a, b}
			}(),
		},
		{
			name: "duplicate mac",
			nics: func() []map[string]any {
				a := validNIC()
				b := validNIC()
				a["device_order"] = 0
				b["device_order"] = 1
				b["mac"] = goodMAC
				return []map[string]any{a, b}
			}(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// withPool=true so that, absent edge validation, the bad NIC
			// would reach 202 and fail deep in the goroutine. A 400 here
			// proves the API edge rejected it.
			fake := &netfabric.FakeFabric{}
			h := newCreateHarness(t, fake, true)

			body, _ := json.Marshal(map[string]any{
				"name":       "vm1",
				"vcpus":      2,
				"memory_mib": 1024,
				"pool":       "default",
				"image_url":  testImageURL,
				"nics":       tc.nics,
			})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/vms", bytes.NewReader(body))

			h.Create(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
			}
			var env response.ErrorBody
			if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env.Error.Code != response.CodeValidationFailed {
				t.Errorf("error.code = %q, want %q", env.Error.Code, response.CodeValidationFailed)
			}
			// Nothing must be materialised on bad input: edge validation
			// runs before the manager touches the fabric.
			if len(fake.CreateTapCalls) != 0 {
				t.Errorf("CreateTapCalls = %v, want none (edge rejected before materialise)", fake.CreateTapCalls)
			}
		})
	}
}

func TestCreate_ValidNIC_PassesValidation(t *testing.T) {
	fake := &netfabric.FakeFabric{}
	h := newCreateHarness(t, fake, true)

	body, _ := json.Marshal(map[string]any{
		"name":       "vm1",
		"vcpus":      2,
		"memory_mib": 1024,
		"pool":       "default",
		"image_url":  testImageURL,
		"nics": []map[string]any{{
			"id":           uuid.NewString(),
			"bridge":       "br0",
			"mac":          "52:54:00:12:34:56",
			"model":        "virtio",
			"mtu":          1500,
			"device_order": 0,
		}},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/vms", bytes.NewReader(body))

	h.Create(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body=%s)", rec.Code, rec.Body.String())
	}

	// Drain the async create goroutine to a terminal task state so its
	// filesystem writes finish before t.TempDir cleanup runs (the stub
	// image is not a real qcow2, so the task fails - that is fine; we
	// only assert NIC validation passed, evidenced by the 202).
	var acc asyncAccepted
	if err := json.Unmarshal(rec.Body.Bytes(), &acc); err != nil {
		t.Fatalf("decode accepted: %v", err)
	}
	taskID := uuid.MustParse(acc.TaskID)
	deadline := time.Now().Add(5 * time.Second)
	for {
		task := h.manager.Task(taskID)
		if task != nil && (task.Status == vm.TaskStatusSuccess || task.Status == vm.TaskStatusFailed) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("async create task did not reach a terminal state")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestCreate_DuplicateUUID_ReAcceptsOriginalTask drives a second POST with
// the same VM uuid (a redelivery before the CP persisted the
// agent_task_id) and confirms the agent re-accepts with 202 echoing the
// ORIGINAL task id rather than minting a fresh one or re-materialising the
// VM. The CP worker then resumes against the same agent_task_id.
func TestCreate_DuplicateUUID_ReAcceptsOriginalTask(t *testing.T) {
	fake := &netfabric.FakeFabric{}
	h := newCreateHarness(t, fake, true)

	vmID := uuid.NewString()
	body, _ := json.Marshal(map[string]any{
		"uuid":       vmID,
		"name":       "vm1",
		"vcpus":      2,
		"memory_mib": 1024,
		"pool":       "default",
		"image_url":  testImageURL,
	})

	post := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/vms", bytes.NewReader(body))
		h.Create(rec, req)
		return rec
	}

	first := post()
	if first.Code != http.StatusAccepted {
		t.Fatalf("first create status = %d, want 202 (body=%s)", first.Code, first.Body.String())
	}
	var firstAcc asyncAccepted
	if err := json.Unmarshal(first.Body.Bytes(), &firstAcc); err != nil {
		t.Fatalf("decode first accepted: %v", err)
	}

	// Drain the original create to a terminal state (the stub image is
	// not a real qcow2 so it fails — fine; the VM and its task record
	// remain registered, which is exactly the redelivery window).
	firstTaskID := uuid.MustParse(firstAcc.TaskID)
	deadline := time.Now().Add(5 * time.Second)
	for {
		task := h.manager.Task(firstTaskID)
		if task != nil && (task.Status == vm.TaskStatusSuccess || task.Status == vm.TaskStatusFailed) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first create task did not reach a terminal state")
		}
		time.Sleep(5 * time.Millisecond)
	}

	second := post()
	if second.Code != http.StatusAccepted {
		t.Fatalf("duplicate create status = %d, want 202 re-accept (body=%s)", second.Code, second.Body.String())
	}
	var secondAcc asyncAccepted
	if err := json.Unmarshal(second.Body.Bytes(), &secondAcc); err != nil {
		t.Fatalf("decode second accepted: %v", err)
	}
	if secondAcc.TaskID != firstAcc.TaskID {
		t.Errorf("duplicate create task_id = %s, want original %s", secondAcc.TaskID, firstAcc.TaskID)
	}
}

// TestCreateMapsNetworkConfig confirms the handler threads the
// network_config wire field into vm.CreateSpec.NetworkData. The harness
// runs over a real manager (there is no spec-capturing fake), so the
// seam is observed through the manager: a non-empty NetworkData makes
// needsCidata true, which synchronously sets the VM's CidataPath during
// Create (before the async image-ensure fails on the unreachable host).
// An empty CidataPath would mean NetworkConfig never reached the spec.
func TestCreateMapsNetworkConfig(t *testing.T) {
	fake := &netfabric.FakeFabric{}
	h := newCreateHarness(t, fake, true)

	vmID := uuid.New()
	body, _ := json.Marshal(map[string]any{
		"uuid":           vmID.String(),
		"name":           "vm1",
		"vcpus":          2,
		"memory_mib":     1024,
		"pool":           "default",
		"image_url":      testImageURL,
		"network_config": "network:\n  version: 2\n",
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/vms", bytes.NewReader(body))

	h.Create(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body=%s)", rec.Code, rec.Body.String())
	}

	v, err := h.manager.Get(vmID)
	if err != nil {
		t.Fatalf("manager.Get(%s): %v", vmID, err)
	}
	if v.CidataPath == "" {
		t.Errorf("CidataPath = %q, want non-empty (network_config must reach CreateSpec.NetworkData)", v.CidataPath)
	}

	// Drain the async create goroutine to a terminal state so its
	// filesystem writes finish before t.TempDir cleanup runs (the stub
	// image is not a real qcow2, so the task fails - that is fine).
	var acc asyncAccepted
	if err := json.Unmarshal(rec.Body.Bytes(), &acc); err != nil {
		t.Fatalf("decode accepted: %v", err)
	}
	taskID := uuid.MustParse(acc.TaskID)
	deadline := time.Now().Add(5 * time.Second)
	for {
		task := h.manager.Task(taskID)
		if task != nil && (task.Status == vm.TaskStatusSuccess || task.Status == vm.TaskStatusFailed) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("async create task did not reach a terminal state")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestCreate_ImageFieldsReachManager confirms the handler threads the new
// image wire fields (image_url / expected_sha256) into vm.CreateSpec: a
// body missing image_url and a body carrying a malformed expected_sha256
// both surface the manager's ErrInvalidSpec as 400 validation_failed.
// This is the seam check - the manager only rejects those because the
// handler copied the decoded fields into the spec it passed.
func TestCreate_ImageFieldsReachManager(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
	}{
		{
			name: "missing image_url",
			body: map[string]any{
				"name": "vm1", "vcpus": 2, "memory_mib": 1024, "pool": "default",
			},
		},
		{
			name: "malformed expected_sha256",
			body: map[string]any{
				"name": "vm1", "vcpus": 2, "memory_mib": 1024, "pool": "default",
				"image_url": testImageURL, "expected_sha256": "deadbeef",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newCreateHarness(t, &netfabric.FakeFabric{}, true)
			body, _ := json.Marshal(tc.body)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/vms", bytes.NewReader(body))

			h.Create(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
			}
			var env response.ErrorBody
			if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env.Error.Code != response.CodeValidationFailed {
				t.Errorf("error.code = %q, want %q", env.Error.Code, response.CodeValidationFailed)
			}
		})
	}
}
