// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/middleware"
)

func TestRequestID_GeneratesWhenAbsent(t *testing.T) {
	var seen string
	h := middleware.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = middleware.RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rec, req)

	if seen == "" {
		t.Fatal("request id missing on context")
	}
	if _, err := uuid.Parse(seen); err != nil {
		t.Errorf("generated id is not a valid uuid: %q (%v)", seen, err)
	}
	if got := rec.Header().Get(middleware.HeaderRequestID); got != seen {
		t.Errorf("response header = %q, want %q", got, seen)
	}
}

func TestRequestID_PreservesIncoming(t *testing.T) {
	const incoming = "client-supplied-id-123"
	var seen string
	h := middleware.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = middleware.RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(middleware.HeaderRequestID, incoming)
	h.ServeHTTP(rec, req)

	if seen != incoming {
		t.Errorf("ctx id = %q, want %q", seen, incoming)
	}
	if got := rec.Header().Get(middleware.HeaderRequestID); got != incoming {
		t.Errorf("response header = %q, want %q", got, incoming)
	}
}

func TestRequestIDFromContext_Empty(t *testing.T) {
	if got := middleware.RequestIDFromContext(context.Background()); got != "" {
		t.Errorf("empty context id = %q, want empty string", got)
	}
}
