// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package response

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBlockingResourcesError_Error(t *testing.T) {
	e := &BlockingResourcesError{
		Message:   "network is in use",
		Resources: map[string]int64{"vm_nics": 3},
	}
	got := e.Error()
	if got == "" {
		t.Fatalf("Error() = %q, want non-empty", got)
	}
}

func TestWriteBlockingResources(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/networks/abc", nil)

	WriteBlockingResources(rec, req, &BlockingResourcesError{
		Message:   "network is in use by virtual machine NICs",
		Resources: map[string]int64{"vm_nics": 2},
	})

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != contentTypeJSON {
		t.Errorf("Content-Type = %q, want %q", got, contentTypeJSON)
	}

	var body ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error.Code != CodeConflict {
		t.Errorf("code = %q, want %q", body.Error.Code, CodeConflict)
	}
	if body.Error.Message != "network is in use by virtual machine NICs" {
		t.Errorf("message = %q", body.Error.Message)
	}
	br, ok := body.Error.Details["blocking_resources"].(map[string]any)
	if !ok {
		t.Fatalf("blocking_resources missing or wrong type: %#v", body.Error.Details)
	}
	if got, _ := br["vm_nics"].(float64); got != 2 {
		t.Errorf("vm_nics = %v, want 2", br["vm_nics"])
	}
}

// TestWriteBlockingResources_CodeOverride locks in the helper
// behaviour: endpoint-specific codes (resource_in_use) take
// precedence over the default `conflict` when the caller sets
// BlockingResourcesError.Code, and Kind surfaces in details.
func TestWriteBlockingResources_CodeOverride(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/storage-pools/abc", nil)

	WriteBlockingResources(rec, req, &BlockingResourcesError{
		Code:      CodeResourceInUse,
		Kind:      "pool",
		Message:   "storage pool is in use by virtual machine disks",
		Resources: map[string]int64{"vm_disks": 3},
	})

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	var body ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error.Code != CodeResourceInUse {
		t.Errorf("code = %q, want %q", body.Error.Code, CodeResourceInUse)
	}
	if got, _ := body.Error.Details["kind"].(string); got != "pool" {
		t.Errorf("details.kind = %v, want \"pool\"", body.Error.Details["kind"])
	}
}
