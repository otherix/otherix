// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package template_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTemplateDelete_Happy(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		if r.URL.Path != "/v1/templates/ubuntu-jammy" {
			t.Errorf("path = %s, want /v1/templates/ubuntu-jammy", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	stdout, _, err := runTemplateCmd(t, srv.URL, []string{"delete", "ubuntu-jammy"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(stdout, "template ubuntu-jammy deleted") {
		t.Errorf("stdout missing success message: %q", stdout)
	}
}

func TestTemplateDelete_BlockedTextOutput(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"resource_in_use","message":"template still has materialised storage images; delete them first","details":{"blocking_resources":{"storage_images":3,"vms":1},"kind":"template"}}}`))
	}))
	defer srv.Close()

	stdout, _, err := runTemplateCmd(t, srv.URL, []string{"delete", "ubuntu-jammy"})
	if err == nil {
		t.Fatalf("expected error для blocked delete")
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Errorf("err missing 'blocked': %v", err)
	}
	if !strings.Contains(stdout, "cannot delete template ubuntu-jammy") {
		t.Errorf("stdout missing header: %q", stdout)
	}
	if !strings.Contains(stdout, "storage_images: 3") {
		t.Errorf("stdout missing storage_images count: %q", stdout)
	}
	if !strings.Contains(stdout, "vms: 1") {
		t.Errorf("stdout missing vms count: %q", stdout)
	}
	// Lexicographic ordering: storage_images < vms — should appear first.
	siIdx := strings.Index(stdout, "storage_images")
	vmIdx := strings.Index(stdout, "vms:")
	if siIdx == -1 || vmIdx == -1 || siIdx >= vmIdx {
		t.Errorf("resource listing not sorted lexicographically: %q", stdout)
	}
}

func TestTemplateDelete_BlockedJSONOutput(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"resource_in_use","message":"x","details":{"blocking_resources":{"storage_images":2}}}}`))
	}))
	defer srv.Close()

	stdout, _, err := runTemplateCmd(t, srv.URL, []string{"delete", "ubuntu-jammy", "--output", "json"})
	if err == nil {
		t.Fatalf("expected error для blocked delete")
	}
	var obj map[string]any
	if jsonErr := json.Unmarshal([]byte(stdout), &obj); jsonErr != nil {
		t.Fatalf("decode json output: %v\nstdout=%q", jsonErr, stdout)
	}
	if obj["deleted"] != false {
		t.Errorf("deleted = %v, want false", obj["deleted"])
	}
	if obj["code"] != "resource_in_use" {
		t.Errorf("code = %v, want resource_in_use", obj["code"])
	}
	if br, ok := obj["blocking_resources"].(map[string]any); !ok {
		t.Errorf("blocking_resources missing or wrong shape: %T", obj["blocking_resources"])
	} else if br["storage_images"].(float64) != 2 {
		t.Errorf("storage_images = %v, want 2", br["storage_images"])
	}
}

func TestTemplateDelete_NotFound(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"template not found"}}`))
	}))
	defer srv.Close()

	_, _, err := runTemplateCmd(t, srv.URL, []string{"delete", "missing"})
	if err == nil {
		t.Fatalf("expected error для 404")
	}
	if !strings.Contains(err.Error(), "not_found") {
		t.Errorf("err = %v, want mention of not_found", err)
	}
}
