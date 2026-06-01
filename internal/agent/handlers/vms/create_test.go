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
	"os"
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

// newCreateHarness builds a Handler over a real manager and a spy
// fabric. When withPool is true a pool plus a stub template file are
// materialised so a request that clears NIC validation reaches 202.
func newCreateHarness(t *testing.T, fake *netfabric.FakeFabric, withPool bool) (*Handler, string) {
	t.Helper()
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	poolRoot := filepath.Join(tmp, "pool")
	const poolName = "default"
	const checksum = "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"

	cfg := &config.AgentConfig{StatePath: stateDir}
	m, err := vm.New(cfg, fake, discardLogger())
	if err != nil {
		t.Fatalf("vm.New: %v", err)
	}
	if withPool {
		if err := m.AddPool(poolName, poolRoot); err != nil {
			t.Fatalf("AddPool: %v", err)
		}
		tmpl := filepath.Join(poolRoot, "templates", checksum+".qcow2")
		if err := os.WriteFile(tmpl, []byte("stub"), 0o600); err != nil {
			t.Fatalf("write template: %v", err)
		}
	}
	return New(m, console.NewTokenStore(), discardLogger()), checksum
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
			h, checksum := newCreateHarness(t, fake, true)

			body, _ := json.Marshal(map[string]any{
				"name":              "vm1",
				"vcpus":             2,
				"memory_mb":         1024,
				"pool":              "default",
				"template_checksum": checksum,
				"nics":              tc.nics,
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
	h, checksum := newCreateHarness(t, fake, true)

	body, _ := json.Marshal(map[string]any{
		"name":              "vm1",
		"vcpus":             2,
		"memory_mb":         1024,
		"pool":              "default",
		"template_checksum": checksum,
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
	// template is not a real qcow2, so the task fails - that is fine; we
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
