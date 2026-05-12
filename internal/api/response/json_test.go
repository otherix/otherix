// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package response_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/otherix/otherix/internal/api/response"
)

func TestWriteJSON(t *testing.T) {
	type payload struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	response.WriteJSON(rec, req, http.StatusOK, payload{Name: "alpha", Count: 3})

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("content-type = %q, want application/json; charset=utf-8", got)
	}

	var got payload
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	want := payload{Name: "alpha", Count: 3}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("body mismatch (-want +got):\n%s", diff)
	}
}

func TestWriteJSON_NilBody(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	response.WriteJSON(rec, req, http.StatusAccepted, nil)

	if rec.Code != http.StatusAccepted {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", rec.Body.String())
	}
}

func TestWriteNoContent(t *testing.T) {
	rec := httptest.NewRecorder()

	response.WriteNoContent(rec)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", rec.Body.String())
	}
}
