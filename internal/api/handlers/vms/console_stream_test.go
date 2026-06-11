// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/store"
)

// consoleStreamStoreStub satisfies the handler's Store interface for the
// ConsoleStream rejection tests. It embeds Store (nil) so only VMByName
// has a body; any other call panics, which proves the handler bailed
// before touching the rest of the store.
type consoleStreamStoreStub struct {
	Store
	vmByNameErr error
}

func (s *consoleStreamStoreStub) VMByName(context.Context, string) (store.VM, error) {
	return store.VM{}, s.vmByNameErr
}

// consoleClientStub satisfies consoleClient so the handler's nil-check
// passes. The rejection paths under test return before any dial, so
// both methods are inert.
type consoleClientStub struct{}

func (consoleClientStub) IssueConsoleToken(context.Context, string, string, string) (agentclient.IssueConsoleTokenResponse, error) {
	return agentclient.IssueConsoleTokenResponse{}, errors.New("consoleClientStub: not implemented")
}

func (consoleClientStub) HTTPClient() *http.Client { return http.DefaultClient }

func consoleStreamHandler(s Store) *Handler {
	return New(s,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		LifecycleDeps{},
		ConsoleDeps{AgentClient: consoleClientStub{}, AccessMode: "proxy"})
}

// consoleStreamRequest builds a GET request carrying the chi route
// context so chi.URLParam(r, "id") resolves to vmName inside the
// handler. token == "" omits the query parameter entirely.
func consoleStreamRequest(vmName, token string) *http.Request {
	target := "/v1/vms/" + url.PathEscape(vmName) + "/console-stream"
	if token != "" {
		target += "?token=" + url.QueryEscape(token)
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", vmName)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// assertGenericConsoleRejection asserts the single response shape every
// unauthenticated-reachable rejection on the anonymous console-stream
// endpoint must produce: 401 unauthenticated with the fixed generic
// message. Identical shapes are the whole point - any divergence is a
// VM-name enumeration oracle.
func assertGenericConsoleRejection(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	var body response.ErrorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if body.Error.Code != response.CodeUnauthenticated {
		t.Errorf("error.code = %q, want %q", body.Error.Code, response.CodeUnauthenticated)
	}
	const want = "invalid or expired console session"
	if body.Error.Message != want {
		t.Errorf("error.message = %q, want %q", body.Error.Message, want)
	}
}

// TestConsoleStreamUnknownVMIsGeneric401 is the enumeration-teeth test:
// an unknown VM name on the anonymous endpoint must produce the same
// 401 a rejected token produces, not a 404 that confirms the name does
// not exist.
func TestConsoleStreamUnknownVMIsGeneric401(t *testing.T) {
	t.Parallel()
	h := consoleStreamHandler(&consoleStreamStoreStub{vmByNameErr: store.ErrNotFound})

	rec := httptest.NewRecorder()
	h.ConsoleStream(rec, consoleStreamRequest("ghost", "bogus-token"))

	assertGenericConsoleRejection(t, rec)
}

// TestConsoleStreamMissingTokenIsGeneric401 locks the missing-token
// branch to the same generic rejection shape.
func TestConsoleStreamMissingTokenIsGeneric401(t *testing.T) {
	t.Parallel()
	h := consoleStreamHandler(&consoleStreamStoreStub{vmByNameErr: store.ErrNotFound})

	rec := httptest.NewRecorder()
	h.ConsoleStream(rec, consoleStreamRequest("ghost", ""))

	assertGenericConsoleRejection(t, rec)
}

// TestRelayUpstreamDialErrorTokenRejectedIsGeneric locks the
// agent-rejected-token relay (upstream 401) to the same generic
// rejection shape as the unknown-VM and missing-token branches.
func TestRelayUpstreamDialErrorTokenRejectedIsGeneric(t *testing.T) {
	t.Parallel()
	h := consoleStreamHandler(&consoleStreamStoreStub{})

	rec := httptest.NewRecorder()
	h.relayUpstreamDialError(rec, consoleStreamRequest("demo", "tok"),
		&http.Response{StatusCode: http.StatusUnauthorized},
		errors.New("upgrade failed"))

	assertGenericConsoleRejection(t, rec)
}

// TestBuildAgentConsoleURLSafeInputsUnchanged pins the historical wire
// format: URL-safe inputs (base64url token, DNS-label VM name) must
// emit byte-identical URLs after the escaping hardening.
func TestBuildAgentConsoleURLSafeInputsUnchanged(t *testing.T) {
	t.Parallel()
	got := buildAgentConsoleURL("agent.example.com:9090", "demo", "tok-123")
	want := "wss://agent.example.com:9090/v1/vms/demo/console-stream?token=tok-123"
	if got != want {
		t.Errorf("buildAgentConsoleURL(safe) = %q, want %q", got, want)
	}
}

// TestBuildAgentConsoleURLEscapesNameAndToken drives inputs that need
// percent-encoding and asserts the token round-trips through net/url
// and the VM name lands escaped in the path.
func TestBuildAgentConsoleURLEscapesNameAndToken(t *testing.T) {
	t.Parallel()
	const rawToken = "a b+c/d=="
	got := buildAgentConsoleURL("agent.example.com:9090", "demo vm", rawToken)

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", got, err)
	}
	if tok := u.Query().Get("token"); tok != rawToken {
		t.Errorf("token round-trip = %q, want %q", tok, rawToken)
	}
	wantPath := "/v1/vms/demo%20vm/console-stream"
	if u.EscapedPath() != wantPath {
		t.Errorf("escaped path = %q, want %q", u.EscapedPath(), wantPath)
	}
	want := "wss://agent.example.com:9090/v1/vms/demo%20vm/console-stream?token=a+b%2Bc%2Fd%3D%3D"
	if got != want {
		t.Errorf("buildAgentConsoleURL = %q, want %q", got, want)
	}
}
