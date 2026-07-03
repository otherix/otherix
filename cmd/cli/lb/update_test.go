// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package lb_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// lbUpdateResponse writes a minimal valid load-balancer view so the update
// command's render path succeeds; the tests assert on the captured request
// body, not the response.
func lbUpdateResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": uuid.NewString(), "name": "web", "owner_id": uuid.NewString(),
		"port": 80, "selector": map[string]any{"app": "web"}, "backends": []any{},
		"created_at": "2026-07-01T10:00:00Z", "updated_at": "2026-07-01T10:00:00Z",
	})
}

// TestLbUpdate_PublishPort asserts `lb update --publish-port N --source-cidr C`
// PATCHes published_port and source_cidrs.
func TestLbUpdate_PublishPort(t *testing.T) {
	t.Parallel()
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s, want PATCH", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		lbUpdateResponse(w)
	}))
	defer srv.Close()

	if _, _, err := runLbCmd(t, srv.URL, []string{
		"update", "web", "--publish-port", "9443", "--source-cidr", "203.0.113.0/24",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotBody["published_port"] != float64(9443) {
		t.Errorf("published_port = %v, want 9443", gotBody["published_port"])
	}
	if got, _ := gotBody["source_cidrs"].([]any); len(got) != 1 {
		t.Errorf("source_cidrs = %v, want one entry", gotBody["source_cidrs"])
	}
}

// TestLbUpdate_NoPublishClears asserts `lb update --no-publish` PATCHes
// published_port=0, the unpublish sentinel.
func TestLbUpdate_NoPublishClears(t *testing.T) {
	t.Parallel()
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		lbUpdateResponse(w)
	}))
	defer srv.Close()

	if _, _, err := runLbCmd(t, srv.URL, []string{"update", "web", "--no-publish"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, present := gotBody["published_port"]; !present {
		t.Fatalf("published_port absent, want 0: %#v", gotBody)
	}
	if gotBody["published_port"] != float64(0) {
		t.Errorf("published_port = %v, want 0", gotBody["published_port"])
	}
}

// TestLbUpdate_NoPublishAndPublishPortConflict asserts the two flags are
// mutually exclusive.
func TestLbUpdate_NoPublishAndPublishPortConflict(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server hit, want a client-side validation error before any request")
		lbUpdateResponse(w)
	}))
	defer srv.Close()

	_, _, err := runLbCmd(t, srv.URL, []string{
		"update", "web", "--no-publish", "--publish-port", "9443",
	})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("err = %v, want a mutual-exclusion error", err)
	}
}
