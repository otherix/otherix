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

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/middleware"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// withUser returns a request whose context carries u as the authn'd
// principal — mirroring what middleware.Authn does in production.
func withUser(u *auth.User) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/anything", nil)
	if u == nil {
		return r
	}
	return r.WithContext(auth.WithUser(r.Context(), u))
}

func TestRequirePermission_Allowed(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})

	user := &auth.User{ID: uuid.New(), Role: auth.RoleAdmin, Type: auth.TypeJWT}
	h := middleware.RequirePermission(auth.PermVMRead, discardLogger())(next)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withUser(user))

	if !called {
		t.Errorf("downstream handler not called for permitted request")
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
}

func TestRequirePermission_Denied(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		called = true
	})

	user := &auth.User{ID: uuid.New(), Role: auth.RoleViewer, Type: auth.TypeJWT}
	h := middleware.RequirePermission(auth.PermUserManage, discardLogger())(next)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withUser(user))

	if called {
		t.Errorf("downstream handler called despite permission denial")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}

	var body response.ErrorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error.Code != response.CodePermissionDenied {
		t.Errorf("code = %q, want %q", body.Error.Code, response.CodePermissionDenied)
	}
	if got := body.Error.Details["required_permission"]; got != string(auth.PermUserManage) {
		t.Errorf("details.required_permission = %v, want %q", got, auth.PermUserManage)
	}
}

func TestRequirePermission_NoUserInContext(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		called = true
	})

	h := middleware.RequirePermission(auth.PermVMRead, discardLogger())(next)

	rec := httptest.NewRecorder()
	// Context without a User — Authn middleware not applied upstream.
	h.ServeHTTP(rec, withUser(nil))

	if called {
		t.Errorf("downstream handler called without authn'd user")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}

	var body response.ErrorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error.Code != response.CodeUnauthenticated {
		t.Errorf("code = %q, want %q", body.Error.Code, response.CodeUnauthenticated)
	}
}

func TestRequirePermission_LogsDeniedAtWarn(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	uid := uuid.New()
	user := &auth.User{ID: uid, Role: auth.RoleViewer, Type: auth.TypeJWT}
	h := middleware.RequirePermission(auth.PermUserManage, log)(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))

	req := withUser(user)
	req.Header.Set(middleware.HeaderRequestID, "req-abc")
	// Wrap with RequestID so RequestIDFromContext returns the header value.
	wrapped := middleware.RequestID(h)
	wrapped.ServeHTTP(httptest.NewRecorder(), req)

	logs, err := io.ReadAll(&buf)
	if err != nil {
		t.Fatalf("read buf: %v", err)
	}
	got := string(logs)
	wantSubstrings := []string{
		`"level":"WARN"`,
		`"msg":"permission denied"`,
		`"user_id":"` + uid.String() + `"`,
		`"role":"viewer"`,
		`"required_permission":"user:manage"`,
		`"path":"/v1/anything"`,
		`"request_id":"req-abc"`,
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(got, s) {
			t.Errorf("log missing %q; full log: %s", s, got)
		}
	}
}

func TestRequirePermission_DeniedDoesNotLogAllowed(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	user := &auth.User{ID: uuid.New(), Role: auth.RoleAdmin, Type: auth.TypeJWT}
	h := middleware.RequirePermission(auth.PermVMRead, log)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	h.ServeHTTP(httptest.NewRecorder(), withUser(user))

	if strings.Contains(buf.String(), "permission denied") {
		t.Errorf("allowed request emitted denied log: %s", buf.String())
	}
}
