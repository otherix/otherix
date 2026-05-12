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

func TestWriteError(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		code     response.ErrorCode
		message  string
		details  map[string]any
		wantBody response.ErrorBody
	}{
		{
			name:    "validation error with details",
			status:  http.StatusBadRequest,
			code:    response.CodeValidationFailed,
			message: "Field 'email' is required",
			details: map[string]any{"field": "email"},
			wantBody: response.ErrorBody{
				Error: response.ErrorDetails{
					Code:    response.CodeValidationFailed,
					Message: "Field 'email' is required",
					Details: map[string]any{"field": "email"},
				},
			},
		},
		{
			name:    "internal error without details",
			status:  http.StatusInternalServerError,
			code:    response.CodeInternal,
			message: "internal server error",
			details: nil,
			wantBody: response.ErrorBody{
				Error: response.ErrorDetails{
					Code:    response.CodeInternal,
					Message: "internal server error",
				},
			},
		},
		{
			name:    "not found",
			status:  http.StatusNotFound,
			code:    response.CodeNotFound,
			message: "resource not found",
			wantBody: response.ErrorBody{
				Error: response.ErrorDetails{
					Code:    response.CodeNotFound,
					Message: "resource not found",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", nil)

			response.WriteError(rec, req, tt.status, tt.code, tt.message, tt.details)

			if got := rec.Code; got != tt.status {
				t.Errorf("status = %d, want %d", got, tt.status)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
				t.Errorf("content-type = %q, want application/json; charset=utf-8", got)
			}

			var got response.ErrorBody
			if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if diff := cmp.Diff(tt.wantBody, got); diff != "" {
				t.Errorf("body mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestWriteError_OmitsEmptyDetails(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	response.WriteError(rec, req, http.StatusInternalServerError, response.CodeInternal, "boom", nil)

	var raw map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&raw); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	errObj, ok := raw["error"].(map[string]any)
	if !ok {
		t.Fatalf("error key missing or wrong type: %#v", raw["error"])
	}
	if _, present := errObj["details"]; present {
		t.Errorf("details key should be omitted when nil, got %#v", errObj["details"])
	}
}
