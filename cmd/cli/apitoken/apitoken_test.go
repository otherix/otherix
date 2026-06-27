// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package apitoken_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/otherix/otherix/cmd/cli/apitoken"
	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
)

// runCmd executes the api-token subcommand tree against args, mounting it
// on a throwaway parent exposing the same persistent flags the real root
// provides.
func runCmd(t *testing.T, endpoint string, stdin io.Reader, args []string) (stdout, stderr string, err error) {
	t.Helper()
	parent := apitoken.NewCommand()
	parent.PersistentFlags().String(cliauth.FlagConfig, "", "")
	parent.PersistentFlags().String(cliauth.FlagEndpoint, "", "")
	parent.PersistentFlags().String(cliauth.FlagToken, "", "")
	parent.PersistentFlags().String(cliauth.FlagCluster, "", "")

	full := append([]string{"--endpoint", endpoint, "--token", "test-token"}, args...)
	parent.SetArgs(full)
	parent.SilenceUsage = true
	parent.SilenceErrors = true
	var out, errBuf bytes.Buffer
	parent.SetOut(&out)
	parent.SetErr(&errBuf)
	if stdin != nil {
		parent.SetIn(stdin)
	}
	parent.SetContext(context.Background())
	err = parent.Execute()
	return out.String(), errBuf.String(), err
}

func TestCreate_Happy(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["name"] != "ci" {
			t.Errorf("name = %v, want ci", body["name"])
		}
		if _, present := body["expires_at"]; present {
			t.Errorf("expires_at should be absent without --ttl, got %v", body["expires_at"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"id1","user_id":"u1","name":"ci","prefix":"otx_ab12","token":"otx_secret_plaintext","created_at":"2026-06-27T10:00:00Z"}`))
	}))
	defer srv.Close()

	stdout, _, err := runCmd(t, srv.URL, nil, []string{"create", "ci"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotPath != "/v1/users/me/api-tokens" {
		t.Errorf("path = %s, want /v1/users/me/api-tokens", gotPath)
	}
	if !strings.Contains(stdout, "otx_secret_plaintext") {
		t.Errorf("create output must show the plaintext token once:\n%s", stdout)
	}
	if !strings.Contains(stdout, "will not be shown again") {
		t.Errorf("create output missing the shown-once warning:\n%s", stdout)
	}
}

func TestCreate_TTLSendsFutureExpiry(t *testing.T) {
	var expiresAt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if v, ok := body["expires_at"].(string); ok {
			expiresAt = v
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"id1","user_id":"u1","name":"ci","prefix":"otx_ab12","token":"otx_x","created_at":"2026-06-27T10:00:00Z"}`))
	}))
	defer srv.Close()

	if _, _, err := runCmd(t, srv.URL, nil, []string{"create", "ci", "--ttl", "90d"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	exp, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		t.Fatalf("expires_at %q is not RFC3339: %v", expiresAt, err)
	}
	want := time.Now().Add(90 * 24 * time.Hour)
	if d := exp.Sub(want); d < -2*time.Minute || d > 2*time.Minute {
		t.Errorf("expires_at = %v, want ~%v (90d out)", exp, want)
	}
}

func TestCreate_UserResolvesToAdminOnBehalfPath(t *testing.T) {
	var createPath, listPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/users":
			listPath = r.URL.Path
			if got := r.URL.Query().Get("username"); got != "alice" {
				t.Errorf("username filter = %q, want alice", got)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[{"id":"u-alice","username":"alice","role":"developer","created_at":"2026-06-01T10:00:00Z","updated_at":"2026-06-01T10:00:00Z"}],"meta":{"next_cursor":null}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/users/u-alice/api-tokens":
			createPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"id1","user_id":"u-alice","name":"ci","prefix":"otx_ab12","token":"otx_x","created_at":"2026-06-27T10:00:00Z"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	if _, _, err := runCmd(t, srv.URL, nil, []string{"create", "ci", "--user", "alice"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if listPath == "" {
		t.Errorf("--user should resolve the username via the users list")
	}
	if createPath != "/v1/users/u-alice/api-tokens" {
		t.Errorf("create path = %s, want /v1/users/u-alice/api-tokens", createPath)
	}
}

// TestCreate_UserPermissionDenied: a role without user:read using --user
// hits a 403 on the username lookup (GET /v1/users) before any token
// route. The CLI surfaces a clean "not found or not permitted" message
// and never POSTs a token.
func TestCreate_UserPermissionDenied(t *testing.T) {
	var tokenPosted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/users":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"permission_denied","message":"user:read required"}}`))
		case r.Method == http.MethodPost:
			tokenPosted = true
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer srv.Close()

	_, _, err := runCmd(t, srv.URL, nil, []string{"create", "ci", "--user", "alice"})
	if err == nil {
		t.Fatalf("expected an error for --user without permission")
	}
	if !strings.Contains(err.Error(), "not found or not permitted") {
		t.Errorf("err = %v, want a clean not-found-or-not-permitted message", err)
	}
	if tokenPosted {
		t.Errorf("no token should be minted when the --user lookup is refused")
	}
}

// TestCreate_NoTokenFlag is the security property: the plaintext is never
// an input flag/argument; it only appears in create OUTPUT. The create
// command must not expose a local --token flag.
func TestCreate_NoTokenFlag(t *testing.T) {
	cmd, _, err := apitoken.NewCommand().Find([]string{"create"})
	if err != nil {
		t.Fatalf("find create subcommand: %v", err)
	}
	if f := cmd.Flags().Lookup("token"); f != nil {
		t.Fatalf("create must NOT expose a --token flag that takes a secret value; found %v", f)
	}
}

func TestList_Table(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/users/me/api-tokens" {
			t.Errorf("path = %s, want /v1/users/me/api-tokens", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[
			{"id":"id1","user_id":"u1","name":"ci","prefix":"otx_ab12","created_at":"2026-06-27T10:00:00Z"},
			{"id":"id2","user_id":"u1","name":"deploy","prefix":"otx_cd34","revoked_at":"2026-06-20T10:00:00Z","created_at":"2026-06-19T10:00:00Z"}
		],"meta":{"next_cursor":null}}`))
	}))
	defer srv.Close()

	stdout, _, err := runCmd(t, srv.URL, nil, []string{"list"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, want := range []string{"PREFIX", "NAME", "STATUS", "otx_ab12", "ci", "active", "otx_cd34", "revoked"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("table missing %q:\n%s", want, stdout)
		}
	}
}

func TestList_IncludeRevokedQuery(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[],"meta":{"next_cursor":null}}`))
	}))
	defer srv.Close()

	if _, _, err := runCmd(t, srv.URL, nil, []string{"list", "--include-revoked"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(gotQuery, "include_revoked=true") {
		t.Errorf("query = %q, want include_revoked=true", gotQuery)
	}
}

func TestList_UserAdminOnBehalf(t *testing.T) {
	var tokenListPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/users":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[{"id":"u-alice","username":"alice","role":"developer","created_at":"2026-06-01T10:00:00Z","updated_at":"2026-06-01T10:00:00Z"}],"meta":{"next_cursor":null}}`))
		case "/v1/users/u-alice/api-tokens":
			tokenListPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[],"meta":{"next_cursor":null}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	if _, _, err := runCmd(t, srv.URL, nil, []string{"list", "--user", "alice"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if tokenListPath != "/v1/users/u-alice/api-tokens" {
		t.Errorf("token list path = %s, want /v1/users/u-alice/api-tokens", tokenListPath)
	}
}

func TestRevoke_ByPrefixResolvesThenDeletes(t *testing.T) {
	var listed, deletedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/users/me/api-tokens":
			listed = r.URL.RawQuery
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[{"id":"id-77","user_id":"u1","name":"ci","prefix":"otx_ab12","created_at":"2026-06-27T10:00:00Z"}],"meta":{"next_cursor":null}}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/users/me/api-tokens/id-77":
			deletedPath = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	stdout, _, err := runCmd(t, srv.URL, nil, []string{"revoke", "otx_ab12", "--force"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(listed, "include_revoked=true") {
		t.Errorf("resolve list should set include_revoked=true, got query %q", listed)
	}
	if deletedPath != "/v1/users/me/api-tokens/id-77" {
		t.Errorf("delete path = %s, want /v1/users/me/api-tokens/id-77", deletedPath)
	}
	if !strings.Contains(stdout, "revoked") {
		t.Errorf("stdout missing revoke confirmation:\n%s", stdout)
	}
}

func TestRevoke_ByFullIDSkipsList(t *testing.T) {
	var listCalls int
	var deletedPath string
	id := "11111111-2222-3333-4444-555555555555"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			listCalls++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[],"meta":{"next_cursor":null}}`))
		case http.MethodDelete:
			deletedPath = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	if _, _, err := runCmd(t, srv.URL, nil, []string{"revoke", id, "--force"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if listCalls != 0 {
		t.Errorf("revoke by full id should not list, got %d list calls", listCalls)
	}
	if deletedPath != "/v1/users/me/api-tokens/"+id {
		t.Errorf("delete path = %s, want .../%s", deletedPath, id)
	}
}

func TestRevoke_AmbiguousPrefixErrorsNoDelete(t *testing.T) {
	var deleted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted = true
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[
			{"id":"id-a","user_id":"u1","name":"ci","prefix":"otx_ab12","created_at":"2026-06-27T10:00:00Z"},
			{"id":"id-b","user_id":"u1","name":"ci2","prefix":"otx_ab12","created_at":"2026-06-27T10:00:00Z"}
		],"meta":{"next_cursor":null}}`))
	}))
	defer srv.Close()

	_, _, err := runCmd(t, srv.URL, nil, []string{"revoke", "otx_ab12", "--force"})
	if err == nil {
		t.Fatalf("expected an ambiguity error")
	}
	if !strings.Contains(err.Error(), "full token id") {
		t.Errorf("err = %v, want guidance to pass the full token id", err)
	}
	if deleted {
		t.Errorf("ambiguous prefix must not delete anything")
	}
}

func TestRevoke_AlreadyRevokedIsNoOp(t *testing.T) {
	var deleted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted = true
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"id-x","user_id":"u1","name":"ci","prefix":"otx_ab12","revoked_at":"2026-06-20T10:00:00Z","created_at":"2026-06-19T10:00:00Z"}],"meta":{"next_cursor":null}}`))
	}))
	defer srv.Close()

	stdout, _, err := runCmd(t, srv.URL, nil, []string{"revoke", "otx_ab12", "--force"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if deleted {
		t.Errorf("already-revoked token must not be deleted again")
	}
	if !strings.Contains(stdout, "already revoked") {
		t.Errorf("stdout should report the no-op:\n%s", stdout)
	}
}

func TestRevoke_NoMatchErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[],"meta":{"next_cursor":null}}`))
	}))
	defer srv.Close()

	_, _, err := runCmd(t, srv.URL, nil, []string{"revoke", "otx_zzzz", "--force"})
	if err == nil || !strings.Contains(err.Error(), "no api token with prefix") {
		t.Fatalf("err = %v, want a clean no-match message", err)
	}
}

// TestRevoke_StalledCursorTerminates proves the prefix-resolution loop
// terminates even when the server keeps returning the same non-empty
// next_cursor (a misbehaving CP). Without the non-advancing-cursor guard
// this would spin forever and the test would hit the go-test timeout.
func TestRevoke_StalledCursorTerminates(t *testing.T) {
	var pages int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			t.Errorf("no token matches the prefix; nothing should be deleted")
		}
		pages++
		if pages > 100 {
			t.Fatalf("resolution loop did not terminate (%d pages)", pages)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Non-matching data with a stable, non-empty cursor every page.
		_, _ = w.Write([]byte(`{"data":[{"id":"id1","user_id":"u1","name":"ci","prefix":"otx_ab12","created_at":"2026-06-27T10:00:00Z"}],"meta":{"next_cursor":"stuck"}}`))
	}))
	defer srv.Close()

	_, _, err := runCmd(t, srv.URL, nil, []string{"revoke", "otx_zzzz", "--force"})
	if err == nil || !strings.Contains(err.Error(), "no api token with prefix") {
		t.Fatalf("err = %v, want a clean no-match message after the loop terminates", err)
	}
}
