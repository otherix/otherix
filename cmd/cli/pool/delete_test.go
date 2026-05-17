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
)

func TestPoolDelete_Happy(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		if r.URL.Path != "/v1/storage-pools/pool-mvp" {
			t.Errorf("path = %s, want /v1/storage-pools/pool-mvp", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	stdout, _, err := runPoolCmd(t, srv.URL, []string{"delete", "pool-mvp", "--force"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(stdout, "pool pool-mvp deleted") {
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
		_, _ = w.Write([]byte(`{"error":{"code":"resource_in_use","message":"storage pool still has materialised storage images; delete them first","details":{"blocking_resources":{"storage_images":3,"vm_disks":1},"kind":"pool"}}}`))
	}))
	defer srv.Close()

	stdout, _, err := runPoolCmd(t, srv.URL, []string{"delete", "pool-mvp", "--force"})
	if err == nil {
		t.Fatalf("expected error для blocked delete")
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Errorf("err missing 'blocked': %v", err)
	}
	if !strings.Contains(stdout, "cannot delete pool pool-mvp") {
		t.Errorf("stdout missing header: %q", stdout)
	}
	if !strings.Contains(stdout, "storage_images: 3") {
		t.Errorf("stdout missing storage_images count: %q", stdout)
	}
	if !strings.Contains(stdout, "vm_disks: 1") {
		t.Errorf("stdout missing vm_disks count: %q", stdout)
	}
	// Lexicographic ordering: storage_images < vm_disks — should appear first.
	siIdx := strings.Index(stdout, "storage_images")
	vmIdx := strings.Index(stdout, "vm_disks")
	if siIdx == -1 || vmIdx == -1 || siIdx >= vmIdx {
		t.Errorf("resource listing not sorted lexicographically: %q", stdout)
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

	stdout, _, err := runPoolCmd(t, srv.URL, []string{"delete", "pool-mvp", "--force", "--output", "json"})
	if err == nil {
		t.Fatalf("expected error для blocked delete")
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
		t.Fatalf("expected error для 404")
	}
	if !strings.Contains(err.Error(), "not_found") {
		t.Errorf("err = %v, want mention of not_found", err)
	}
}

// TestPoolDelete_NoForceOnPipedStdin verifies the confirmation prompt
// is skipped when stdin is not а TTY (e.g. CI / script redirection).
// Without --force и без TTY → delete proceeds. Mirror VM delete UX.
func TestPoolDelete_NoForceOnPipedStdin(t *testing.T) {
	t.Parallel()
	// Test harness uses а non-TTY stdin by default (го test pipe),
	// so the no-prompt branch fires automatically — exercises the
	// "automation runs через без prompt" path.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	_, _, err := runPoolCmd(t, srv.URL, []string{"delete", "pool-mvp"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
}
