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

const deleteManifest = `apiVersion: otherix/v1
kind: Network
metadata: { name: net-mvp }
spec: { type: bridge, bridgeName: br0 }
---
apiVersion: otherix/v1
kind: StoragePool
metadata: { name: pool-mvp }
spec: { path: /opt/p, node: node-1 }
---
apiVersion: otherix/v1
kind: VM
metadata: { name: web-1 }
spec: { imageURL: https://x/u.qcow2, arch: arm64 }
`

func TestDeleteReverseOrderResolvesPool(t *testing.T) {
	instID := uuid.NewString()
	netID := uuid.NewString()
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/storage-pools/pool-mvp":
			_, _ = w.Write([]byte(`{"name":"pool-mvp","instances":[{"id":"` + instID + `","name":"pool-mvp","node":"node-1","type":"local_dir","path":"/opt/p"}]}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/storage-pools/"+instID:
			w.WriteHeader(http.StatusNoContent)
		// DeleteNetwork resolves the name to a UUID via the list endpoint
		// before issuing the UUID-keyed DELETE.
		case r.Method == http.MethodGet && r.URL.Path == "/v1/networks":
			_, _ = w.Write([]byte(`{"data":[{"id":"` + netID + `","name":"net-mvp","type":"bridge","bridge_name":"br0"}],"meta":{"next_cursor":null}}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/networks/"+netID:
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/vms/web-1":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"task_id":"` + uuid.NewString() + `","status":"pending","links":{"self":"/v1/tasks/x"}}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	_, _, err := runRoot(t, srv.URL, "delete", "-f", writeManifest(t, deleteManifest), "--force")
	if err != nil {
		t.Fatalf("delete -f error = %v", err)
	}
	if calls[0] != "DELETE /v1/vms/web-1" {
		t.Errorf("first delete = %q, want DELETE /v1/vms/web-1; calls=%v", calls[0], calls)
	}
	if calls[len(calls)-1] != "DELETE /v1/networks/"+netID {
		t.Errorf("last delete = %q, want DELETE /v1/networks/%s; calls=%v", calls[len(calls)-1], netID, calls)
	}
}

func TestDeleteDryRunIssuesNoCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("dry-run must not issue HTTP calls")
	}))
	defer srv.Close()
	stdout, _, err := runRoot(t, srv.URL, "delete", "-f", writeManifest(t, deleteManifest), "--dry-run")
	if err != nil {
		t.Fatalf("dry-run error = %v", err)
	}
	if !strings.Contains(stdout, "web-1") || !strings.Contains(stdout, "pool-mvp") {
		t.Errorf("dry-run plan = %q, want it to list the targets", stdout)
	}
}

func TestDeleteReportsBlockerNonZeroExit(t *testing.T) {
	netID := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// DeleteNetwork resolves the name to a UUID first, then the
		// UUID-keyed DELETE returns the 409 blocker.
		if r.Method == http.MethodGet && r.URL.Path == "/v1/networks" {
			_, _ = w.Write([]byte(`{"data":[{"id":"` + netID + `","name":"net-mvp","type":"bridge","bridge_name":"br0"}],"meta":{"next_cursor":null}}`))
			return
		}
		if r.Method == http.MethodDelete && r.URL.Path == "/v1/networks/"+netID {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"network in use","details":{"blocking_resources":{"vm_nics":2}}}}`))
			return
		}
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"name":"pool-mvp","instances":[{"id":"` + uuid.NewString() + `","name":"pool-mvp","node":"node-1","type":"local_dir","path":"/opt/p"}]}`))
			return
		}
		if r.URL.Path == "/v1/vms/web-1" {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"task_id":"x","status":"pending","links":{"self":"/v1/tasks/x"}}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	_, _, err := runRoot(t, srv.URL, "delete", "-f", writeManifest(t, deleteManifest), "--force")
	if err == nil {
		t.Fatalf("expected non-nil error when a delete is blocked")
	}
}
