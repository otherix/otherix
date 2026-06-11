// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package middleware_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/otherix/otherix/internal/api/middleware"
)

// readAllHandler reads the full request body and records the bytes read
// and the read error so tests can assert on both.
type readAllHandler struct {
	body []byte
	err  error
}

func (h *readAllHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.body, h.err = io.ReadAll(r.Body)
	w.WriteHeader(http.StatusOK)
}

func TestMaxBodyBytes_UnderLimitReadsFully(t *testing.T) {
	h := &readAllHandler{}
	wrapped := middleware.MaxBodyBytes(64)(h)

	body := []byte("hello body under the limit")
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body)))

	if h.err != nil {
		t.Errorf("io.ReadAll error = %v, want nil", h.err)
	}
	if !bytes.Equal(h.body, body) {
		t.Errorf("body read = %q, want %q", h.body, body)
	}
}

func TestMaxBodyBytes_OverLimitFailsRead(t *testing.T) {
	h := &readAllHandler{}
	wrapped := middleware.MaxBodyBytes(16)(h)

	body := strings.Repeat("x", 64)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))

	if h.err == nil {
		t.Fatalf("io.ReadAll error = nil, want *http.MaxBytesError")
	}
	var maxErr *http.MaxBytesError
	if !errors.As(h.err, &maxErr) {
		t.Errorf("io.ReadAll error = %v (%T), want *http.MaxBytesError", h.err, h.err)
	}
}

func TestMaxBodyBytes_NonPositiveLimitIsPassThrough(t *testing.T) {
	for _, limit := range []int64{0, -1} {
		h := &readAllHandler{}
		wrapped := middleware.MaxBodyBytes(limit)(h)

		// Larger than any limit a wrapping MaxBytesReader could have
		// silently applied with these inputs.
		body := strings.Repeat("y", 4096)
		rec := httptest.NewRecorder()
		wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))

		if h.err != nil {
			t.Errorf("limit %d: io.ReadAll error = %v, want nil (pass-through)", limit, h.err)
		}
		if got := len(h.body); got != len(body) {
			t.Errorf("limit %d: read %d bytes, want %d", limit, got, len(body))
		}
	}
}
