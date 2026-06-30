// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package sshgrant_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	"github.com/otherix/otherix/cmd/cli/sshgrant"
)

// runCmd executes the ssh-grant subcommand tree against args, mounting it on a
// throwaway parent exposing the same persistent flags the real root provides.
func runCmd(t *testing.T, endpoint string, args []string) (stdout, stderr string, err error) {
	t.Helper()
	parent := sshgrant.NewCommand()
	parent.PersistentFlags().String(cliauth.FlagConfig, "", "")
	parent.PersistentFlags().String(cliauth.FlagEndpoint, "", "")
	parent.PersistentFlags().String(cliauth.FlagToken, "", "")
	parent.PersistentFlags().String(cliauth.FlagCluster, "", "")

	// Point --config at a nonexistent path so the test never picks up the
	// developer's real ~/.otherix/config (which could carry a cluster CA
	// bundle and skew the derived bundle trust). Load treats a missing file
	// as an empty config, so endpoint/token come solely from the flags.
	emptyConfig := filepath.Join(t.TempDir(), "config")
	full := append([]string{"--config", emptyConfig, "--endpoint", endpoint, "--token", "test-token"}, args...)
	parent.SetArgs(full)
	parent.SilenceUsage = true
	parent.SilenceErrors = true
	var out, errBuf bytes.Buffer
	parent.SetOut(&out)
	parent.SetErr(&errBuf)
	parent.SetContext(context.Background())
	err = parent.Execute()
	return out.String(), errBuf.String(), err
}

const createReply = `{"id":"g-1","name":"alice-web","recipient_label":"Alice",` +
	`"created_by":"u-1","vms":[{"vm_name":"web01","login":"deploy"},{"vm_name":"web02","login":"deploy"}],` +
	`"expires_at":"2026-07-07T10:00:00Z","revoked":false,` +
	`"created_at":"2026-06-30T10:00:00Z","updated_at":"2026-06-30T10:00:00Z","token":"otx_sshgrant_PLAINTEXT"}`

func TestCreate_PostsBodyAndEmitsBundle(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(createReply))
	}))
	defer srv.Close()

	stdout, _, err := runCmd(t, srv.URL, []string{
		"create", "alice-web", "--vm", "web01,web02", "--login", "deploy", "--ttl", "168h", "--user", "Alice",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/ssh-grants" {
		t.Errorf("request = %s %s, want POST /v1/ssh-grants", gotMethod, gotPath)
	}
	if gotBody["name"] != "alice-web" || gotBody["ttl"] != "168h" || gotBody["recipient_label"] != "Alice" {
		t.Errorf("body = %v, want name/ttl/recipient_label set", gotBody)
	}
	vms, ok := gotBody["vms"].([]any)
	if !ok || len(vms) != 2 {
		t.Fatalf("body vms = %v, want 2 entries", gotBody["vms"])
	}
	first := vms[0].(map[string]any)
	if first["vm_name"] != "web01" || first["login"] != "deploy" {
		t.Errorf("first vm = %v, want web01/deploy", first)
	}

	// The text output must carry a paste-able bundle blob; decoding it must
	// surface the token, server, trust, and the granted vm:login set.
	blob := extractBlob(t, stdout)
	bundle, err := sshgrant.ParseBundle(blob)
	if err != nil {
		t.Fatalf("ParseBundle from create output: %v\n%s", err, stdout)
	}
	if bundle.Token != "otx_sshgrant_PLAINTEXT" {
		t.Errorf("bundle token = %q, want the one-time plaintext", bundle.Token)
	}
	if bundle.ServerURL != srv.URL {
		t.Errorf("bundle server = %q, want %q", bundle.ServerURL, srv.URL)
	}
	if bundle.Trust != sshgrant.TrustWebPKI {
		t.Errorf("bundle trust = %q, want webpki (plain-http flag endpoint)", bundle.Trust)
	}
	wantVMs := map[string]string{"web01": "deploy", "web02": "deploy"}
	got := map[string]string{}
	for _, vm := range bundle.VMs {
		got[vm.VM] = vm.Login
	}
	for k, v := range wantVMs {
		if got[k] != v {
			t.Errorf("bundle vm %s login = %q, want %q", k, got[k], v)
		}
	}
}

func TestCreate_OmitsTTLWhenUnset(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(createReply))
	}))
	defer srv.Close()

	if _, _, err := runCmd(t, srv.URL, []string{"create", "alice-web", "--vm", "web01"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, present := gotBody["ttl"]; present {
		t.Errorf("ttl must be omitted without --ttl, got %v", gotBody["ttl"])
	}
}

func TestCreate_InlinePerVMLogin(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(createReply))
	}))
	defer srv.Close()

	if _, _, err := runCmd(t, srv.URL, []string{"create", "g", "--vm", "db01=postgres,web01"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	vms := gotBody["vms"].([]any)
	db := vms[0].(map[string]any)
	web := vms[1].(map[string]any)
	if db["vm_name"] != "db01" || db["login"] != "postgres" {
		t.Errorf("db entry = %v, want db01/postgres", db)
	}
	if web["vm_name"] != "web01" || web["login"] != "root" {
		t.Errorf("web entry = %v, want web01/root (default login)", web)
	}
}

func TestCreate_JSONFormEmitsBundle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(createReply))
	}))
	defer srv.Close()

	stdout, _, err := runCmd(t, srv.URL, []string{"create", "alice-web", "--vm", "web01", "-o", "json"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var b sshgrant.Bundle
	if err := json.Unmarshal([]byte(stdout), &b); err != nil {
		t.Fatalf("create -o json is not a Bundle: %v\n%s", err, stdout)
	}
	if b.Version != sshgrant.BundleVersion || b.Token != "otx_sshgrant_PLAINTEXT" {
		t.Errorf("json bundle = %+v, want versioned bundle with token", b)
	}
}

func TestCreate_NoVMErrors(t *testing.T) {
	_, _, err := runCmd(t, "http://127.0.0.1:0", []string{"create", "g"})
	if err == nil || !strings.Contains(err.Error(), "at least one VM") {
		t.Fatalf("err = %v, want an at-least-one-VM error", err)
	}
}

func TestCreate_ConflictIsCleanMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"name in use"}}`))
	}))
	defer srv.Close()

	_, _, err := runCmd(t, srv.URL, []string{"create", "dupe", "--vm", "web01"})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v, want an already-exists message", err)
	}
}

func TestList_Table(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ssh-grants" {
			t.Errorf("path = %s, want /v1/ssh-grants", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[
			{"id":"g-1","name":"alice-web","recipient_label":"Alice","created_by":"u-1",
			 "vms":[{"vm_name":"web01","login":"deploy"}],"expires_at":null,"revoked":false,
			 "created_at":"2026-06-30T10:00:00Z","updated_at":"2026-06-30T10:00:00Z"},
			{"id":"g-2","name":"old","recipient_label":"","created_by":"u-1",
			 "vms":[],"expires_at":null,"revoked":true,
			 "created_at":"2026-06-29T10:00:00Z","updated_at":"2026-06-29T10:00:00Z"}
		],"meta":{"next_cursor":null}}`))
	}))
	defer srv.Close()

	stdout, _, err := runCmd(t, srv.URL, []string{"list"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, want := range []string{"NAME", "RECIPIENT", "VMS", "STATUS", "alice-web", "web01:deploy", "active", "revoked", "never"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("table missing %q:\n%s", want, stdout)
		}
	}
}

func TestGet_ByNameResolvesThenGetsByID(t *testing.T) {
	var listed, gotByID bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/ssh-grants":
			listed = true
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[{"id":"g-77","name":"alice-web","recipient_label":"Alice",
				"created_by":"u-1","vms":[{"vm_name":"web01","login":"deploy"}],"expires_at":null,"revoked":false,
				"created_at":"2026-06-30T10:00:00Z","updated_at":"2026-06-30T10:00:00Z"}],"meta":{"next_cursor":null}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/ssh-grants/g-77":
			gotByID = true
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"g-77","name":"alice-web","recipient_label":"Alice",
				"created_by":"u-1","vms":[{"vm_name":"web01","login":"deploy"}],"expires_at":null,"revoked":false,
				"created_at":"2026-06-30T10:00:00Z","updated_at":"2026-06-30T10:00:00Z"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	stdout, _, err := runCmd(t, srv.URL, []string{"get", "alice-web"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !listed || !gotByID {
		t.Errorf("get by name should list then get by id (listed=%v gotByID=%v)", listed, gotByID)
	}
	if !strings.Contains(stdout, "alice-web") || !strings.Contains(stdout, "web01") {
		t.Errorf("get output missing grant details:\n%s", stdout)
	}
}

func TestAddVM_PostsToVMsSubresource(t *testing.T) {
	id := "11111111-2222-3333-4444-555555555555"
	var gotPath, gotMethod string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"` + id + `","name":"g","recipient_label":"","created_by":"u-1",
			"vms":[{"vm_name":"db01","login":"postgres"}],"expires_at":null,"revoked":false,
			"created_at":"2026-06-30T10:00:00Z","updated_at":"2026-06-30T10:00:00Z"}`))
	}))
	defer srv.Close()

	stdout, _, err := runCmd(t, srv.URL, []string{"add-vm", id, "db01", "--login", "postgres"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/ssh-grants/"+id+"/vms" {
		t.Errorf("request = %s %s, want POST /v1/ssh-grants/%s/vms", gotMethod, gotPath, id)
	}
	if gotBody["vm_name"] != "db01" || gotBody["login"] != "postgres" {
		t.Errorf("body = %v, want db01/postgres", gotBody)
	}
	if !strings.Contains(stdout, "added db01") {
		t.Errorf("missing add confirmation:\n%s", stdout)
	}
}

func TestRemoveVM_DeletesVMSubresource(t *testing.T) {
	id := "11111111-2222-3333-4444-555555555555"
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"` + id + `","name":"g","recipient_label":"","created_by":"u-1",
			"vms":[],"expires_at":null,"revoked":false,
			"created_at":"2026-06-30T10:00:00Z","updated_at":"2026-06-30T10:00:00Z"}`))
	}))
	defer srv.Close()

	stdout, _, err := runCmd(t, srv.URL, []string{"remove-vm", id, "web01"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/v1/ssh-grants/"+id+"/vms/web01" {
		t.Errorf("request = %s %s, want DELETE /v1/ssh-grants/%s/vms/web01", gotMethod, gotPath, id)
	}
	if !strings.Contains(stdout, "removed web01") {
		t.Errorf("missing remove confirmation:\n%s", stdout)
	}
}

func TestRevoke_PostsRevoke(t *testing.T) {
	id := "11111111-2222-3333-4444-555555555555"
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"` + id + `","name":"g","recipient_label":"","created_by":"u-1",
			"vms":[],"expires_at":null,"revoked":true,
			"created_at":"2026-06-30T10:00:00Z","updated_at":"2026-06-30T10:00:00Z"}`))
	}))
	defer srv.Close()

	stdout, _, err := runCmd(t, srv.URL, []string{"revoke", id})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/ssh-grants/"+id+"/revoke" {
		t.Errorf("request = %s %s, want POST /v1/ssh-grants/%s/revoke", gotMethod, gotPath, id)
	}
	if !strings.Contains(stdout, "revoked") {
		t.Errorf("missing revoke confirmation:\n%s", stdout)
	}
}

// extractBlob pulls the otx_sshbundle_ line out of the create text output.
func extractBlob(t *testing.T, stdout string) string {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "otx_sshbundle_") {
			return line
		}
	}
	t.Fatalf("no otx_sshbundle_ blob in create output:\n%s", stdout)
	return ""
}
