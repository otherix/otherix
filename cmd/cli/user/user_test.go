// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package user_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	"github.com/otherix/otherix/cmd/cli/user"
)

// runUserCmd executes the `user` cobra subcommand tree against args,
// mounting it on a throwaway parent that exposes the same persistent
// flags the real root provides. stdin, when non-nil, is wired as the
// command's input so the --password-stdin path reads from it (and the
// password therefore never appears in argv).
func runUserCmd(t *testing.T, endpoint string, stdin io.Reader, args []string) (stdout, stderr string, err error) {
	t.Helper()
	parent := user.NewCommand()
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

// userJSON emits a minimal userView projection the CLI can decode.
func userJSON(id, username, role string) []byte {
	body := map[string]any{
		"id":         id,
		"username":   username,
		"role":       role,
		"created_at": "2026-06-01T10:00:00Z",
		"updated_at": "2026-06-01T10:00:00Z",
	}
	raw, _ := json.Marshal(body)
	return raw
}

func TestUserCreate_Happy(t *testing.T) {
	t.Parallel()
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/users" {
			t.Errorf("path = %s, want /v1/users", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["username"] != "web-admin" {
			t.Errorf("username = %v, want web-admin", body["username"])
		}
		if body["role"] != "developer" {
			t.Errorf("role = %v, want developer", body["role"])
		}
		if body["password"] != "s3cret-password" {
			t.Errorf("password = %v, want the stdin-supplied secret", body["password"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(userJSON(uuid.NewString(), "web-admin", "developer"))
	}))
	defer srv.Close()

	stdout, _, err := runUserCmd(t, srv.URL, strings.NewReader("s3cret-password\n"), []string{
		"create", "web-admin", "--role", "developer", "--password-stdin",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if posts != 1 {
		t.Errorf("posts = %d, want 1", posts)
	}
	if !strings.Contains(stdout, "user web-admin created") {
		t.Errorf("stdout missing creation message: %q", stdout)
	}
}

func TestUserCreate_RequiresRole(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("HTTP call must not happen when --role is missing")
	}))
	defer srv.Close()

	_, _, err := runUserCmd(t, srv.URL, strings.NewReader("pw\n"), []string{
		"create", "web-admin", "--password-stdin",
	})
	if err == nil {
		t.Fatalf("expected error for missing --role")
	}
	if !strings.Contains(err.Error(), "role") {
		t.Errorf("err = %v, want mention of role", err)
	}
}

// TestUserCreate_NoPasswordFlag is the security property: there is no
// --password string flag on create (it would leak the secret into shell
// history / the process table), and the password is supplied via stdin
// so it never sits in argv.
func TestUserCreate_NoPasswordFlag(t *testing.T) {
	t.Parallel()
	cmd, _, err := user.NewCommand().Find([]string{"create"})
	if err != nil {
		t.Fatalf("find create subcommand: %v", err)
	}
	if f := cmd.Flags().Lookup("password"); f != nil {
		t.Fatalf("create must NOT expose a --password flag (leaks into shell history / ps); found %v", f)
	}
	if f := cmd.Flags().Lookup("password-stdin"); f == nil {
		t.Errorf("create must expose --password-stdin for the no-argv password path")
	}
}

// TestUserSetPassword_NoPasswordFlag mirrors the create-side security
// property for set-password.
func TestUserSetPassword_NoPasswordFlag(t *testing.T) {
	t.Parallel()
	cmd, _, err := user.NewCommand().Find([]string{"set-password"})
	if err != nil {
		t.Fatalf("find set-password subcommand: %v", err)
	}
	if f := cmd.Flags().Lookup("password"); f != nil {
		t.Fatalf("set-password must NOT expose a --password flag; found %v", f)
	}
	if f := cmd.Flags().Lookup("password-stdin"); f == nil {
		t.Errorf("set-password must expose --password-stdin")
	}
}

func TestUserGet_JSON(t *testing.T) {
	t.Parallel()
	id := uuid.NewString()
	var listed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// get resolves username -> row via the ?username= guard list.
		if r.URL.Path != "/v1/users" {
			t.Errorf("path = %s, want /v1/users", r.URL.Path)
		}
		if got := r.URL.Query().Get("username"); got != "web-admin" {
			t.Errorf("username filter = %q, want web-admin", got)
		}
		listed = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[` + string(userJSON(id, "web-admin", "developer")) + `],"meta":{"next_cursor":null}}`))
	}))
	defer srv.Close()

	stdout, _, err := runUserCmd(t, srv.URL, nil, []string{"get", "web-admin", "-o", "json"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !listed {
		t.Errorf("get should have resolved via the username filter list")
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not valid json: %v\n%s", err, stdout)
	}
	if got["username"] != "web-admin" {
		t.Errorf("json username = %v, want web-admin", got["username"])
	}
	if got["role"] != "developer" {
		t.Errorf("json role = %v, want developer", got["role"])
	}
}

func TestUserGet_NotFound(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[],"meta":{"next_cursor":null}}`))
	}))
	defer srv.Close()

	_, _, err := runUserCmd(t, srv.URL, nil, []string{"get", "ghost"})
	if err == nil {
		t.Fatalf("expected error for missing user")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v, want a clean not found message", err)
	}
}

func TestUserList_Table(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/users" {
			t.Errorf("path = %s, want /v1/users", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[` + string(userJSON(uuid.NewString(), "alice", "admin")) +
			`,` + string(userJSON(uuid.NewString(), "bob", "viewer")) + `],"meta":{"next_cursor":null}}`))
	}))
	defer srv.Close()

	stdout, _, err := runUserCmd(t, srv.URL, nil, []string{"list"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, want := range []string{"USERNAME", "ROLE", "EMAIL", "DISPLAY_NAME", "CREATED_AT", "alice", "bob"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("table missing %q:\n%s", want, stdout)
		}
	}
}

func TestUserDelete_ResolvesThenDeletes(t *testing.T) {
	t.Parallel()
	id := uuid.NewString()
	var listed, deleted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/users":
			if got := r.URL.Query().Get("username"); got != "web-admin" {
				t.Errorf("username filter = %q, want web-admin", got)
			}
			listed = true
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[` + string(userJSON(id, "web-admin", "developer")) + `],"meta":{"next_cursor":null}}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/users/"+id:
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	stdout, _, err := runUserCmd(t, srv.URL, nil, []string{"delete", "web-admin", "--force"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !listed {
		t.Errorf("delete should have resolved the username first")
	}
	if !deleted {
		t.Errorf("delete should have issued the by-id DELETE")
	}
	if !strings.Contains(stdout, "user web-admin deleted") {
		t.Errorf("stdout missing deletion message: %q", stdout)
	}
}

func TestUserSetRole_PatchesRole(t *testing.T) {
	t.Parallel()
	id := uuid.NewString()
	var listed, patched bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/users":
			listed = true
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[` + string(userJSON(id, "web-admin", "developer")) + `],"meta":{"next_cursor":null}}`))
		case r.Method == http.MethodPatch && r.URL.Path == "/v1/users/"+id:
			patched = true
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode patch body: %v", err)
			}
			if body["role"] != "operator" {
				t.Errorf("patch role = %v, want operator", body["role"])
			}
			if _, present := body["password"]; present {
				t.Errorf("set-role must not send a password field, got %v", body["password"])
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(userJSON(id, "web-admin", "operator"))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	stdout, _, err := runUserCmd(t, srv.URL, nil, []string{"set-role", "web-admin", "operator"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !listed || !patched {
		t.Errorf("set-role should resolve then PATCH (listed=%v patched=%v)", listed, patched)
	}
	if !strings.Contains(stdout, "operator") {
		t.Errorf("stdout missing new role:\n%s", stdout)
	}
}

func TestUserSetPassword_PatchesViaStdin(t *testing.T) {
	t.Parallel()
	id := uuid.NewString()
	var patched bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/users":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[` + string(userJSON(id, "web-admin", "developer")) + `],"meta":{"next_cursor":null}}`))
		case r.Method == http.MethodPatch && r.URL.Path == "/v1/users/"+id:
			patched = true
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode patch body: %v", err)
			}
			if body["password"] != "new-password-123" {
				t.Errorf("patch password = %v, want the stdin-supplied secret", body["password"])
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(userJSON(id, "web-admin", "developer"))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	stdout, _, err := runUserCmd(t, srv.URL, strings.NewReader("new-password-123\n"), []string{
		"set-password", "web-admin", "--password-stdin",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !patched {
		t.Errorf("set-password should have issued the PATCH")
	}
	if !strings.Contains(stdout, "password updated") {
		t.Errorf("stdout missing update message:\n%s", stdout)
	}
}

func TestUserWhoami_GetsMe(t *testing.T) {
	t.Parallel()
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/users/me" {
			t.Errorf("request = %s %s, want GET /v1/users/me", r.Method, r.URL.Path)
		}
		hit = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(userJSON(uuid.NewString(), "admin", "admin"))
	}))
	defer srv.Close()

	stdout, _, err := runUserCmd(t, srv.URL, nil, []string{"whoami"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !hit {
		t.Errorf("whoami should GET /v1/users/me")
	}
	if !strings.Contains(stdout, "admin") {
		t.Errorf("stdout missing username:\n%s", stdout)
	}
}
