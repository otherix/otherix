// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/otherix/otherix/internal/agent/vm"
	"github.com/otherix/otherix/internal/api/response"
)

func TestMapAsyncLifecycleError(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   response.ErrorCode
	}{
		{
			name:       "ErrNotFound maps to 404",
			err:        vm.ErrNotFound,
			wantStatus: http.StatusNotFound,
			wantCode:   response.CodeNotFound,
		},
		{
			name:       "ErrInvalidState maps to 409",
			err:        fmt.Errorf("%w: poweroff requires a past-spawn phase, got %q", vm.ErrInvalidState, "pending"),
			wantStatus: http.StatusConflict,
			wantCode:   response.CodeConflict,
		},
		{
			name:       "ErrInFlight maps to 409 (not 500)",
			err:        vm.ErrInFlight,
			wantStatus: http.StatusConflict,
			wantCode:   response.CodeConflict,
		},
		{
			name:       "ErrInFlight wrapped also maps to 409",
			err:        fmt.Errorf("poweroff: %w", vm.ErrInFlight),
			wantStatus: http.StatusConflict,
			wantCode:   response.CodeConflict,
		},
		{
			name:       "ErrQMPUnavailable maps to 500",
			err:        vm.ErrQMPUnavailable,
			wantStatus: http.StatusInternalServerError,
			wantCode:   response.CodeInternal,
		},
		{
			name:       "unknown error maps to 500",
			err:        errors.New("boom"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   response.CodeInternal,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/vms/foo/poweroff", nil)

			mapAsyncLifecycleError(rec, req, tc.err)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			var body response.ErrorBody
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.Error.Code != tc.wantCode {
				t.Errorf("error.code = %q, want %q", body.Error.Code, tc.wantCode)
			}
		})
	}
}

// TestMapAsyncLifecycleError_InFlightCarriesRetryHint asserts that the
// 409 emitted for an in-flight collision carries the same retry-hint
// envelope as the CP-side idempotency middleware so CLI / SDK clients
// have a single shape to honour on transient lifecycle contention.
func TestMapAsyncLifecycleError_InFlightCarriesRetryHint(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/vms/foo/poweroff", nil)

	mapAsyncLifecycleError(rec, req, vm.ErrInFlight)

	var body response.ErrorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	got, ok := body.Error.Details["retry_after_seconds"]
	if !ok {
		t.Fatalf("details missing retry_after_seconds; got details=%v", body.Error.Details)
	}
	// JSON numbers decode to float64 through map[string]any.
	if n, ok := got.(float64); !ok || n <= 0 {
		t.Errorf("retry_after_seconds = %v (%T), want positive number", got, got)
	}
}
