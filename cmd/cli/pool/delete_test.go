// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package pool_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// writeJSONBody marshals v and writes it as a 200 application/json body.
func writeJSONBody(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal test body: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

func TestPoolDelete_Happy(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		if r.URL.Path != "/v1/storage-pools/pool-dev" {
			t.Errorf("path = %s, want /v1/storage-pools/pool-dev", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	stdout, _, err := runPoolCmd(t, srv.URL, []string{"delete", "pool-dev", "--force"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(stdout, "pool pool-dev deleted") {
		t.Errorf("stdout missing success message: %q", stdout)
	}
}

func TestPoolDelete_HappyByUUID(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/storage-pools/"+id.String() {
			t.Errorf("path = %s, want /v1/storage-pools/<uuid>", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	_, _, err := runPoolCmd(t, srv.URL, []string{"delete", id.String(), "--force"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestPoolDelete_BlockedTextOutput(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"resource_in_use","message":"storage pool is in use by virtual machine disks; remove or migrate them first","details":{"blocking_resources":{"vm_disks":3},"kind":"pool"}}}`))
	}))
	defer srv.Close()

	stdout, _, err := runPoolCmd(t, srv.URL, []string{"delete", "pool-dev", "--force"})
	if err == nil {
		t.Fatalf("expected error for blocked delete")
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Errorf("err missing 'blocked': %v", err)
	}
	if !strings.Contains(stdout, "cannot delete pool pool-dev") {
		t.Errorf("stdout missing header: %q", stdout)
	}
	if !strings.Contains(stdout, "vm_disks: 3") {
		t.Errorf("stdout missing vm_disks count: %q", stdout)
	}
}

func TestPoolDelete_BlockedJSONOutput(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"storage pool is in use by virtual machine disks; remove or migrate them first","details":{"blocking_resources":{"vm_disks":2}}}}`))
	}))
	defer srv.Close()

	stdout, _, err := runPoolCmd(t, srv.URL, []string{"delete", "pool-dev", "--force", "--output", "json"})
	if err == nil {
		t.Fatalf("expected error for blocked delete")
	}
	var obj map[string]any
	if jerr := json.Unmarshal([]byte(stdout), &obj); jerr != nil {
		t.Fatalf("decode json output: %v\nstdout=%q", jerr, stdout)
	}
	if obj["deleted"] != false {
		t.Errorf("deleted = %v, want false", obj["deleted"])
	}
	if obj["code"] != "conflict" {
		t.Errorf("code = %v, want conflict", obj["code"])
	}
	br, ok := obj["blocking_resources"].(map[string]any)
	if !ok {
		t.Fatalf("blocking_resources missing or wrong shape: %T", obj["blocking_resources"])
	}
	if br["vm_disks"].(float64) != 2 {
		t.Errorf("vm_disks = %v, want 2", br["vm_disks"])
	}
}

func TestPoolDelete_NotFound(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"storage pool not found"}}`))
	}))
	defer srv.Close()

	_, _, err := runPoolCmd(t, srv.URL, []string{"delete", "missing", "--force"})
	if err == nil {
		t.Fatalf("expected error for 404")
	}
	if !strings.Contains(err.Error(), "not_found") {
		t.Errorf("err = %v, want mention of not_found", err)
	}
}

// TestPoolDelete_ByNode covers the --node disambiguation path: the CLI
// fetches the pool's per-node instances, picks the one on the named node,
// and deletes it by its instance UUID.
func TestPoolDelete_ByNode(t *testing.T) {
	t.Parallel()
	id1, id2 := uuid.New(), uuid.New()
	var deletedID string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/storage-pools/default", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONBody(t, w, cpclient.PoolConceptView{
			Name: "default", Type: "local_dir",
			Instances: []cpclient.Pool{
				{ID: id1, Node: "node-1", Name: "default", Type: "local_dir", Path: "/p1"},
				{ID: id2, Node: "node-2", Name: "default", Type: "local_dir", Path: "/p2"},
			},
		})
	})
	mux.HandleFunc("DELETE /v1/storage-pools/{id}", func(w http.ResponseWriter, r *http.Request) {
		deletedID = r.PathValue("id")
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	stdout, _, err := runPoolCmd(t, srv.URL, []string{"delete", "default", "--node", "node-1", "--force"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if deletedID != id1.String() {
		t.Errorf("deleted instance = %s, want node-1's %s", deletedID, id1)
	}
	if !strings.Contains(stdout, "pool default on node node-1 deleted") {
		t.Errorf("stdout missing node-scoped success: %q", stdout)
	}
}

// TestPoolDelete_ByNodeMissing covers --node naming a node with no
// instance of the pool: the CLI reports the node and the nodes that do
// carry an instance instead of a bare failure.
func TestPoolDelete_ByNodeMissing(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/storage-pools/default", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONBody(t, w, cpclient.PoolConceptView{
			Name: "default", Type: "local_dir",
			Instances: []cpclient.Pool{{ID: uuid.New(), Node: "node-1", Name: "default"}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, _, err := runPoolCmd(t, srv.URL, []string{"delete", "default", "--node", "node-9", "--force"})
	if err == nil {
		t.Fatal("expected error for --node naming a node with no instance")
	}
	if !strings.Contains(err.Error(), "no instance on node \"node-9\"") || !strings.Contains(err.Error(), "node-1") {
		t.Errorf("err = %v, want mention of node-9 and the available node-1", err)
	}
}

// TestPoolDelete_AmbiguousLists covers the bare-name multi-instance path:
// the server returns pool_name_ambiguous and the CLI lists every per-node
// instance (node + UUID) plus the two retry forms.
func TestPoolDelete_AmbiguousLists(t *testing.T) {
	t.Parallel()
	id1, id2 := uuid.New(), uuid.New()
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /v1/storage-pools/default", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"pool_name_ambiguous","message":"pool name resolves to multiple instances; address by UUID","details":{"name":"default"}}}`))
	})
	mux.HandleFunc("GET /v1/storage-pools/default", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONBody(t, w, cpclient.PoolConceptView{
			Name: "default", Type: "local_dir",
			Instances: []cpclient.Pool{
				{ID: id1, Node: "node-1", Name: "default"},
				{ID: id2, Node: "node-2", Name: "default"},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	stdout, _, err := runPoolCmd(t, srv.URL, []string{"delete", "default", "--force"})
	if err == nil {
		t.Fatal("expected error for ambiguous pool name")
	}
	for _, want := range []string{"node-1", id1.String(), "node-2", id2.String(), "--node <node>"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q\n%s", want, stdout)
		}
	}
}

// TestPoolDelete_BlockedShowsVMs covers the conflict path: a pool blocked
// by vm_disks lists the names of the VMs holding a disk on it, fetched via
// the vm list pool filter.
func TestPoolDelete_BlockedShowsVMs(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	var vmPoolFilter string
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /v1/storage-pools/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"storage pool is in use by virtual machine disks","details":{"blocking_resources":{"vm_disks":2}}}}`))
	})
	mux.HandleFunc("GET /v1/vms", func(w http.ResponseWriter, r *http.Request) {
		vmPoolFilter = r.URL.Query().Get("pool")
		writeJSONBody(t, w, cpclient.VMList{Data: []cpclient.VM{{Name: "smoke-img"}, {Name: "smoke-img2"}}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	stdout, _, err := runPoolCmd(t, srv.URL, []string{"delete", id.String(), "--force"})
	if err == nil {
		t.Fatal("expected error for blocked delete")
	}
	if vmPoolFilter != id.String() {
		t.Errorf("vm list pool filter = %q, want %q", vmPoolFilter, id)
	}
	for _, want := range []string{"vm_disks: 2", "VMs with a disk on this pool:", "smoke-img", "smoke-img2"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q\n%s", want, stdout)
		}
	}
}

// TestPoolDelete_NoForceOnPipedStdin verifies the confirmation prompt
// is skipped when stdin is not a TTY (e.g. CI / script redirection).
// Without --force and without TTY → delete proceeds. Mirror VM delete UX.
func TestPoolDelete_NoForceOnPipedStdin(t *testing.T) {
	t.Parallel()
	// Test harness uses a non-TTY stdin by default (go test pipe),
	// so the no-prompt branch fires automatically — exercises the
	// "automation runs via without prompt" path.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	_, _, err := runPoolCmd(t, srv.URL, []string{"delete", "pool-dev"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
}
