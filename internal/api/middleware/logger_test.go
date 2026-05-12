// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package middleware_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/otherix/otherix/internal/api/middleware"
)

type logRecord struct {
	Level     string  `json:"level"`
	Msg       string  `json:"msg"`
	Method    string  `json:"method"`
	Path      string  `json:"path"`
	Status    int     `json:"status"`
	Bytes     int     `json:"bytes"`
	RequestID string  `json:"request_id"`
	UA        string  `json:"user_agent"`
	Remote    string  `json:"remote_addr"`
	Duration  float64 `json:"duration"`
}

func newCapturingLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestLogger_BasicSuccess(t *testing.T) {
	var buf bytes.Buffer
	log := newCapturingLogger(&buf)

	h := middleware.RequestID(middleware.Logger(log)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("User-Agent", "ua-test")
	h.ServeHTTP(rec, req)

	var rec0 logRecord
	if err := json.Unmarshal(buf.Bytes(), &rec0); err != nil {
		t.Fatalf("decode log: %v\nraw: %s", err, buf.String())
	}
	if rec0.Level != "INFO" {
		t.Errorf("level = %q, want INFO", rec0.Level)
	}
	if rec0.Msg != "http request" {
		t.Errorf("msg = %q, want %q", rec0.Msg, "http request")
	}
	if rec0.Method != http.MethodGet || rec0.Path != "/healthz" {
		t.Errorf("method/path = %q %q, want GET /healthz", rec0.Method, rec0.Path)
	}
	if rec0.Status != http.StatusOK {
		t.Errorf("status = %d, want 200", rec0.Status)
	}
	if rec0.Bytes != len(`{"ok":true}`) {
		t.Errorf("bytes = %d, want %d", rec0.Bytes, len(`{"ok":true}`))
	}
	if rec0.RequestID == "" {
		t.Error("request_id should be populated by RequestID middleware")
	}
	if rec0.UA != "ua-test" {
		t.Errorf("user_agent = %q, want ua-test", rec0.UA)
	}
}

func TestLogger_LevelByStatus(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		wantLevel string
	}{
		{name: "2xx is info", status: http.StatusOK, wantLevel: "INFO"},
		{name: "3xx is info", status: http.StatusFound, wantLevel: "INFO"},
		{name: "4xx is warn", status: http.StatusBadRequest, wantLevel: "WARN"},
		{name: "5xx is error", status: http.StatusInternalServerError, wantLevel: "ERROR"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			log := newCapturingLogger(&buf)
			h := middleware.Logger(log)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			h.ServeHTTP(rec, req)

			var got logRecord
			if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
				t.Fatalf("decode log: %v\nraw: %s", err, buf.String())
			}
			if got.Level != tt.wantLevel {
				t.Errorf("level = %q, want %q", got.Level, tt.wantLevel)
			}
		})
	}
}

func TestLogger_DefaultStatusIs200(t *testing.T) {
	// chi's WrapResponseWriter reports 200 when the handler writes a body
	// without explicitly calling WriteHeader, mirroring net/http behaviour.
	var buf bytes.Buffer
	log := newCapturingLogger(&buf)
	h := middleware.Logger(log)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hi"))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	var got logRecord
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode log: %v\nraw: %s", err, buf.String())
	}
	if got.Status != http.StatusOK {
		t.Errorf("status = %d, want 200", got.Status)
	}
}
