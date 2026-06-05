// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

const waitManifest = `apiVersion: otherix/v1
kind: VM
metadata: { name: web-1 }
spec: { imageURL: https://x/u.qcow2, arch: arm64 }
`

func TestCreateWaitPollsTaskToSuccess(t *testing.T) {
	taskID := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/vms":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"task_id":"` + taskID + `","status":"pending","links":{"self":"/v1/tasks/` + taskID + `"}}`))
		case "/v1/tasks/" + taskID:
			_, _ = w.Write([]byte(`{"id":"` + taskID + `","type":"vm.create","status":"success","progress":null,"resource_type":"vm","resource_id":null,"result":null,"error":null,"attempts":1,"max_attempts":1,"created_at":"2026-06-05T00:00:00Z","started_at":null,"finished_at":null}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	stdout, _, err := runRoot(t, srv.URL, "create", "-f", writeManifest(t, waitManifest), "--wait", "--wait-timeout", "10s")
	if err != nil {
		t.Fatalf("create --wait error = %v", err)
	}
	if !strings.Contains(stdout, "ready") {
		t.Errorf("stdout = %q, want a readiness line", stdout)
	}
}

const waitPoolManifest = `apiVersion: otherix/v1
kind: StoragePool
metadata: { name: pool-1 }
spec: { path: /opt/p, node: node-1 }
`

func TestCreateWaitPoolReconciliation(t *testing.T) {
	poolID := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/storage-pools":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"` + poolID + `","node":"node-1","name":"pool-1","type":"local_dir","path":"/opt/p","reconciliation_status":"pending","config":{},"images":[],"created_at":"2026-06-05T00:00:00Z","updated_at":"2026-06-05T00:00:00Z"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/storage-pools/"+poolID:
			_, _ = w.Write([]byte(`{"id":"` + poolID + `","node":"node-1","name":"pool-1","type":"local_dir","path":"/opt/p","reconciliation_status":"ready","config":{},"images":[],"created_at":"2026-06-05T00:00:00Z","updated_at":"2026-06-05T00:00:00Z"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	stdout, _, err := runRoot(t, srv.URL, "create", "-f", writeManifest(t, waitPoolManifest), "--wait", "--wait-timeout", "10s")
	if err != nil {
		t.Fatalf("create --wait error = %v", err)
	}
	if !strings.Contains(stdout, "ready") {
		t.Errorf("stdout = %q, want a readiness line", stdout)
	}
}
