// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package middleware_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
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
// process. This test guards both the recovery and the wire shape —
// status 500 with the standard error envelope, mirroring Recoverer's
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
// goroutine-boundary divergence from Recoverer: re-panicking from a
// spawned goroutine crashes the process, so Timeout swallows
// http.ErrAbortHandler without logging or writing a response. The
// outer select sees done close and returns with whatever zero-value the
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
		t.Errorf("expected no log output for ErrAbortHandler, got: %s", logBuf.String())
	}
}

// TestTimeout_LateHandlerHeaderWriteDoesNotRaceThe503 pins that a handler still
// setting response headers after the deadline fired cannot race the parent's
// 503 write on the same header map. guardedWriter.Header() must hand out a
// buffered map private to the handler, not the underlying writer's map, so a
// late Header().Set() and the parent's WriteError never touch the same map
// concurrently. Before the fix both mutated the underlying map with no
// synchronisation - a fatal "concurrent map writes" throw that crashes the api
// process. Run under -race to catch the regression.
func TestTimeout_LateHandlerHeaderWriteDoesNotRaceThe503(t *testing.T) {
	handlerDone := make(chan struct{})
	h := middleware.Timeout(10 * time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		// A late handler keeps mutating the response header map while the
		// parent writes the timeout 503 for the very same request.
		for i := 0; i < 2000; i++ {
			w.Header().Set("X-Late", strconv.Itoa(i))
		}
		close(handlerDone)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	<-handlerDone // let the detached goroutine finish before asserting

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

// TestTimeout_HeaderOnlyImplicit200FlushesHeaders pins that a fast handler which
// sets a response header and returns with an implicit 200 and no body still has
// that header on the wire. guardedWriter buffers headers into a private map
// flushed on WriteHeader/Write, so the non-timeout (done) path must flush them
// too, matching net/http.TimeoutHandler - otherwise a header-only response is
// silently dropped.
func TestTimeout_HeaderOnlyImplicit200FlushesHeaders(t *testing.T) {
	h := middleware.Timeout(time.Second)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Custom", "present")
		// Returns with an implicit 200 and no body: never calls WriteHeader/Write.
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if got := rec.Header().Get("X-Custom"); got != "present" {
		t.Errorf("X-Custom = %q, want %q (header-only implicit-200 must flush)", got, "present")
	}
}
