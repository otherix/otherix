// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package middleware_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/api/middleware"
	"github.com/otherix/otherix/internal/api/response"
)

func TestTimeout_FastHandlerPassesThrough(t *testing.T) {
	h := middleware.Timeout(50 * time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != `{"ok":true}` {
		t.Errorf("body = %q, want %q", got, `{"ok":true}`)
	}
}

func TestTimeout_SlowHandlerYields503(t *testing.T) {
	h := middleware.Timeout(20 * time.Millisecond)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}

	var body response.ErrorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error.Code != response.CodeRequestTimeout {
		t.Errorf("code = %q, want %q", body.Error.Code, response.CodeRequestTimeout)
	}
	if body.Error.Message != "request timeout" {
		t.Errorf("message = %q, want %q", body.Error.Message, "request timeout")
	}
}

// TestTimeout_RecoversPanicInSpawnedGoroutine pins the contract that the
// Timeout middleware recovers panics from the goroutine it spawns to
// run the handler. The upstream Recoverer middleware cannot catch
// panics that occur on a different goroutine, so without the recover
// inside Timeout itself a handler panic would crash the whole api
// process. This test guards both the recovery и the wire shape —
// status 500 с the standard error envelope, mirroring Recoverer's
// contract.
func TestTimeout_RecoversPanicInSpawnedGoroutine(t *testing.T) {
	var logBuf bytes.Buffer
	testLogger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	prev := slog.Default()
	slog.SetDefault(testLogger)
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := middleware.Timeout(time.Second)(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("intentional panic for test")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("rec.Code = %d, want 500", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("content-type = %q, want application/json; charset=utf-8", got)
	}
	var body response.ErrorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error.Code != response.CodeInternal {
		t.Errorf("error.code = %q, want %q", body.Error.Code, response.CodeInternal)
	}

	logs := logBuf.String()
	if !strings.Contains(logs, `"msg":"panic in handler (timeout middleware goroutine)"`) {
		t.Errorf("log missing timeout-specific panic message, got: %s", logs)
	}
	if !strings.Contains(logs, `"panic":"intentional panic for test"`) {
		t.Errorf("log missing panic value, got: %s", logs)
	}
	if !strings.Contains(logs, `"stack"`) {
		t.Errorf("log missing stack trace, got: %s", logs)
	}
}

// TestTimeout_PanicWithErrAbortHandlerSilentlyAborts pins the
// goroutine-boundary divergence от Recoverer: re-panicking из а
// spawned goroutine crashes the process, so Timeout swallows
// http.ErrAbortHandler without logging or writing а response. The
// outer select sees done close и returns с whatever zero-value the
// ResponseWriter carries — matching the spirit of the sentinel.
func TestTimeout_PanicWithErrAbortHandlerSilentlyAborts(t *testing.T) {
	var logBuf bytes.Buffer
	testLogger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	prev := slog.Default()
	slog.SetDefault(testLogger)
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := middleware.Timeout(time.Second)(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("rec.Code = %d, want default 200 (silent abort)", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("rec.Body = %q, want empty (silent abort)", rec.Body.String())
	}
	if logBuf.Len() != 0 {
		t.Errorf("expected no log output для ErrAbortHandler, got: %s", logBuf.String())
	}
}
