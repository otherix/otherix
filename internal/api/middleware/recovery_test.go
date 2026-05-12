// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package middleware_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/otherix/otherix/internal/api/middleware"
	"github.com/otherix/otherix/internal/api/response"
)

func TestRecoverer_CatchesPanic(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	h := middleware.Recoverer(log)(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("content-type = %q, want application/json; charset=utf-8", got)
	}

	var body response.ErrorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error.Code != response.CodeInternal {
		t.Errorf("code = %q, want %q", body.Error.Code, response.CodeInternal)
	}

	logs, err := io.ReadAll(&buf)
	if err != nil {
		t.Fatalf("read log buffer: %v", err)
	}
	logStr := string(logs)
	if !strings.Contains(logStr, `"panic":"boom"`) {
		t.Errorf("log missing panic value, got: %s", logStr)
	}
	if !strings.Contains(logStr, `"stack"`) {
		t.Errorf("log missing stack trace, got: %s", logStr)
	}
}

func TestRecoverer_NoPanicPassesThrough(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))

	h := middleware.Recoverer(log)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want 418", rec.Code)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no log output, got: %s", buf.String())
	}
}

func TestRecoverer_PreservesAbortHandlerSentinel(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))

	h := middleware.Recoverer(log)(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	defer func() {
		got := recover()
		if got != http.ErrAbortHandler {
			t.Fatalf("recovered value = %v, want http.ErrAbortHandler", got)
		}
	}()

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	t.Fatal("expected panic to propagate")
}
