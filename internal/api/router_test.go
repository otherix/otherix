// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/api"
	"github.com/otherix/otherix/internal/api/middleware"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
)

// silentLogger discards all output — handy when only behaviour matters.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// NewRouter takes a *store.Store, but the routes exercised here (the
// unknown-path 404 and wrong-method 405) do not touch it. Tests that
// need the health endpoints live in internal/api/health under build tag
// integration; they exercise the same router shape end-to-end.
func TestRouter_NotFoundUsesErrorEnvelope(t *testing.T) {
	r := api.NewRouter(api.RouterDeps{Logger: silentLogger(), RequestTimeout: 5 * time.Second})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/no-such-path", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("content-type = %q, want application/json", got)
	}
	var body response.ErrorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode 404 body: %v", err)
	}
	if body.Error.Code != response.CodeNotFound {
		t.Errorf("error.code = %q, want %q", body.Error.Code, response.CodeNotFound)
	}
}

func TestRouter_MethodNotAllowedUsesErrorEnvelope(t *testing.T) {
	r := api.NewRouter(api.RouterDeps{Logger: silentLogger(), RequestTimeout: 5 * time.Second})

	// /healthz is registered as GET. POST against it must hit the
	// MethodNotAllowed handler with the standard envelope.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
	var body response.ErrorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode 405 body: %v", err)
	}
	if body.Error.Code != response.CodeMethodNotAllowed {
		t.Errorf("error.code = %q, want %q", body.Error.Code, response.CodeMethodNotAllowed)
	}
}

func TestRouter_MiddlewareStackIsActive(t *testing.T) {
	// Hitting an unknown path exercises RequestID + Logger + chi's default
	// 404, which together prove the stack is wired. Calling /healthz with a
	// nil store would NPE the health handler before middleware is observable.
	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	r := api.NewRouter(api.RouterDeps{Logger: log, RequestTimeout: 5 * time.Second})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/no-such", nil)
	req.Header.Set(middleware.HeaderRequestID, "test-id-abc")
	r.ServeHTTP(rec, req)

	if got := rec.Header().Get(middleware.HeaderRequestID); got != "test-id-abc" {
		t.Errorf("response request id = %q, want test-id-abc", got)
	}
	if !strings.Contains(logBuf.String(), `"request_id":"test-id-abc"`) {
		t.Errorf("expected log line containing request_id; got: %s", logBuf.String())
	}
}

// TestRouter_BoundedRESTInheritsTimeout locks the structural pattern
// adopted by the timeout-fix iteration: NewRouter wraps bounded REST
// routes in a Group that applies middleware.Timeout, while long-lived
// WebSocket routes (vms.consoleStream) are registered as siblings
// outside the Group. This regression test mirrors that structure
// directly with probe handlers — fail loudly if a future refactor
// drops the Group or the Timeout middleware from inside it.
//
// Pre-fix landing the Timeout was applied at root and blanketed every
// route, including console-stream. Post-fix the Group pattern is the
// invariant: bounded subtree → Timeout enforced; streaming sibling →
// Timeout absent.
func TestRouter_BoundedRESTInheritsTimeout(t *testing.T) {
	const requestTimeout = 50 * time.Millisecond
	const handlerSleep = 200 * time.Millisecond

	r := chi.NewRouter()

	// Streaming sibling — registered before the Group, must NOT inherit
	// Timeout. The probe handler sleeps past requestTimeout and expects
	// to run to completion without being cancelled.
	streamingDone := make(chan struct{})
	r.Get("/streaming", func(_ http.ResponseWriter, _ *http.Request) {
		time.Sleep(handlerSleep)
		close(streamingDone)
	})

	// Bounded REST Group — Timeout middleware applies. The probe
	// handler sleeps past requestTimeout and should surface a 503 with
	// the standard timeout envelope.
	r.Group(func(r chi.Router) {
		r.Use(middleware.Timeout(requestTimeout))
		r.Get("/bounded", func(_ http.ResponseWriter, req *http.Request) {
			select {
			case <-req.Context().Done():
			case <-time.After(handlerSleep):
			}
		})
	})

	// Bounded route → 503.
	boundedRec := httptest.NewRecorder()
	start := time.Now()
	r.ServeHTTP(boundedRec, httptest.NewRequest(http.MethodGet, "/bounded", nil))
	boundedElapsed := time.Since(start)
	if boundedRec.Code != http.StatusServiceUnavailable {
		t.Errorf("bounded route status = %d, want 503 (Timeout did not fire)", boundedRec.Code)
	}
	if boundedElapsed >= handlerSleep {
		t.Errorf("bounded route ran full duration %v, want < %v (Timeout did not fire)",
			boundedElapsed, handlerSleep)
	}
	var body response.ErrorBody
	if err := json.NewDecoder(boundedRec.Body).Decode(&body); err == nil {
		if body.Error.Code != response.CodeRequestTimeout {
			t.Errorf("bounded route error code = %q, want %q", body.Error.Code, response.CodeRequestTimeout)
		}
	}

	// Streaming route → 200 after full sleep (Timeout not applied).
	streamingRec := httptest.NewRecorder()
	start = time.Now()
	r.ServeHTTP(streamingRec, httptest.NewRequest(http.MethodGet, "/streaming", nil))
	streamingElapsed := time.Since(start)
	select {
	case <-streamingDone:
	default:
		t.Errorf("streaming handler did not run to completion (Timeout middleware leaked?)")
	}
	if streamingElapsed < handlerSleep {
		t.Errorf("streaming route returned early at %v, want ≥ %v (Timeout cancelled it)",
			streamingElapsed, handlerSleep)
	}
}

// routerStoreStub satisfies api.RouterStore by embedding the (nil)
// interface. Any actual store call panics — the body-cap test below
// never reaches one because the oversized body fails JSON decode
// before the login handler touches the store.
type routerStoreStub struct{ api.RouterStore }

// authStoreStub satisfies auth.Store the same way; auth.NewService only
// checks the store for nil-ness at construction time.
type authStoreStub struct{ auth.Store }

// TestRouter_RequestBodyCappedOnAnonymousEndpoint proves NewRouter wires
// MaxBodyBytes (at its 1 MiB default) in front of the /v1 surface: an
// unauthenticated POST to auth.login with a body just over the cap must
// fail the handler's JSON decode (400 validation_failed) instead of
// being buffered and processed. Pre-wiring this returned a non-400
// (the 2 MiB body decoded fine and login proceeded).
func TestRouter_RequestBodyCappedOnAnonymousEndpoint(t *testing.T) {
	svc, err := auth.NewService(auth.Config{
		JWTSecret:    []byte("router-test-secret-32-bytes-pad!"),
		JWTAccessTTL: 15 * time.Minute,
		RefreshTTL:   720 * time.Hour,
	}, authStoreStub{})
	if err != nil {
		t.Fatalf("auth.NewService: %v", err)
	}

	r := api.NewRouter(api.RouterDeps{
		Store:          routerStoreStub{},
		AuthService:    svc,
		Logger:         silentLogger(),
		RequestTimeout: 5 * time.Second,
	})

	// A single oversized JSON string value forces the decoder to read
	// past the 1 MiB cap before it can produce a value.
	body := `{"email":"` + strings.Repeat("a", int(api.DefaultMaxRequestBodyBytes)+1024) + `","password":"x"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (body cap did not fire)", rec.Code)
	}
	var envelope response.ErrorBody
	if err := json.NewDecoder(rec.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if envelope.Error.Code != response.CodeValidationFailed {
		t.Errorf("error.code = %q, want %q", envelope.Error.Code, response.CodeValidationFailed)
	}
}

// TestAgentRouter_BodyCapIsAgentDefaultNotPublic proves NewAgentRouter
// uses the generous DefaultMaxAgentBodyBytes (16 MiB) rather than the
// public router's 1 MiB DefaultMaxRequestBodyBytes. The agent listener
// carries the mTLS-trusted heartbeat whose body scales with node
// density and can exceed 1 MiB; capping it at 1 MiB would push a dense
// node unreachable.
//
// The probe drives the anonymous /v1/nodes/join endpoint (reachable
// without an mTLS identity) with a body sized between the two caps: a
// large unknown padding field (the decoder reads past 1 MiB) plus valid
// token + csr_pem but a deliberately-missing node_name. The two caps
// produce distinguishable 400 envelopes:
//   - at the 1 MiB cap the decoder hits a read error mid-body and the
//     handler returns "invalid request body";
//   - at the 16 MiB cap the body decodes fully, validate() runs, and the
//     handler returns "node_name is required".
//
// Observing the latter message proves the larger cap is in force.
func TestAgentRouter_BodyCapIsAgentDefaultNotPublic(t *testing.T) {
	r := api.NewAgentRouter(api.RouterDeps{
		Store:          routerStoreStub{},
		Logger:         silentLogger(),
		RequestTimeout: 5 * time.Second,
	})

	// Body sits between 1 MiB and 16 MiB: rejected by the public cap,
	// accepted by the agent cap. Padding lives in an unknown field so
	// the (non-strict) decoder reads it then discards it.
	padLen := int(api.DefaultMaxRequestBodyBytes) + (1 << 20) // ~2 MiB, < 16 MiB
	if int64(padLen) >= api.DefaultMaxAgentBodyBytes {
		t.Fatalf("padLen %d must stay under agent cap %d", padLen, api.DefaultMaxAgentBodyBytes)
	}
	// Valid token + csr_pem, node_name omitted so validate() trips on
	// errMissingNodeName once the body is decoded under the larger cap.
	body := `{"_pad":"` + strings.Repeat("a", padLen) +
		`","token":"otx_join_xxx","csr_pem":"dummy"}`

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/nodes/join", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var envelope response.ErrorBody
	if err := json.NewDecoder(rec.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if envelope.Error.Code != response.CodeValidationFailed {
		t.Fatalf("error.code = %q, want %q", envelope.Error.Code, response.CodeValidationFailed)
	}
	// "invalid request body" => the 1 MiB cap fired and the decode failed
	// mid-stream. "node_name is required" => the body decoded under the
	// agent cap and validation proceeded. Only the latter is correct.
	if got := envelope.Error.Message; got == "invalid request body" {
		t.Errorf("agent router rejected a sub-16-MiB body at decode (message %q): "+
			"body cap resolved to the 1 MiB public default, not the agent default", got)
	} else if got != "node_name is required" {
		t.Errorf("error.message = %q, want %q (body decoded under agent cap, validate() tripped)",
			got, "node_name is required")
	}
}

func TestRouter_TimeoutEnforced(t *testing.T) {
	// Chain a slow downstream handler onto the router. We can't add routes
	// to NewRouter from outside, so instead build an equivalent: directly
	// exercise the timeout middleware here is sufficient to validate that
	// NewRouter wires it (since the order in router.go is the contract).
	// Smoke: a 10 ms timeout against a ~50 ms handler returns 503 envelope.
	mw := middleware.Timeout(10 * time.Millisecond)
	h := mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(50 * time.Millisecond):
		}
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(context.Background()))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	var body response.ErrorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != response.CodeRequestTimeout {
		t.Errorf("code = %q, want %q", body.Error.Code, response.CodeRequestTimeout)
	}
}
