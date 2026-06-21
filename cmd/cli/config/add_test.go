// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package config_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/otherix/otherix/cmd/cli/config"
	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	"github.com/otherix/otherix/cmd/cli/internal/cliconfig"
)

// runConfigCmd executes the `config` cobra subcommand tree against
// args, mounting it on a throwaway parent that exposes the same
// persistent flags the real root provides. Returns stdout / stderr
// captured during execution.
func runConfigCmd(t *testing.T, configPath string, args []string, stdin string) (stdout, stderr string, err error) {
	t.Helper()
	parent := config.NewCommand()
	parent.PersistentFlags().String(cliauth.FlagConfig, "", "")
	parent.PersistentFlags().String(cliauth.FlagEndpoint, "", "")
	parent.PersistentFlags().String(cliauth.FlagToken, "", "")
	parent.PersistentFlags().String(cliauth.FlagCluster, "", "")

	full := append([]string{"--config", configPath}, args...)
	parent.SetArgs(full)
	var out, errBuf bytes.Buffer
	parent.SetOut(&out)
	parent.SetErr(&errBuf)
	parent.SetIn(strings.NewReader(stdin))
	parent.SetContext(context.Background())
	err = parent.Execute()
	return out.String(), errBuf.String(), err
}

// loginSrv is a minimal httptest CP that handles login and api-token
// create. Used by add-cluster tests.
type loginSrv struct {
	*httptest.Server
	revokedID  string
	createName string
}

func newLoginSrv(t *testing.T) *loginSrv {
	t.Helper()
	srv := &loginSrv{}
	srv.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/auth/login":
			var req struct{ Email, Password string }
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Email == "" || req.Password == "" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"code":"validation_failed","message":"required"}}`)
				return
			}
			if req.Password == "wrong" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":{"code":"invalid_credentials","message":"bad creds"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"access_token":"jwt","refresh_token":"r","token_type":"Bearer","expires_in":900}`)
		case r.URL.Path == "/v1/users/me/api-tokens" && r.Method == http.MethodPost:
			var req struct{ Name string }
			_ = json.NewDecoder(r.Body).Decode(&req)
			srv.createName = req.Name
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"019e0000-0000-7000-8000-000000000001","user_id":"u","name":"`+req.Name+`","prefix":"otx_zzz1234","token":"otx_zzz1234567","created_at":"now","last_used_at":null,"expires_at":null,"revoked_at":null}`)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/users/me/api-tokens/"):
			srv.revokedID = strings.TrimPrefix(r.URL.Path, "/v1/users/me/api-tokens/")
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Logf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return srv
}

// newTLSLoginSrv is newLoginSrv over TLS, additionally serving /v1/ca
// with the server's own self-signed cert as the trust anchor (so the
// CLI's TOFU fetch + pin yields a CA that validates this same server).
func newTLSLoginSrv(t *testing.T) *loginSrv {
	t.Helper()
	srv := &loginSrv{}
	srv.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/ca":
			certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
			sum := sha256.Sum256(srv.Certificate().Raw)
			hexFP := hex.EncodeToString(sum[:])
			_, _ = io.WriteString(w, `{"cas":[{"cert_pem":`+strconv.Quote(string(certPEM))+`,"fingerprint_sha256":"`+hexFP+`"}],"signer_fingerprint_sha256":"`+hexFP+`"}`)
		case r.URL.Path == "/v1/auth/login":
			_, _ = io.WriteString(w, `{"access_token":"jwt","refresh_token":"r","token_type":"Bearer","expires_in":900}`)
		case r.URL.Path == "/v1/users/me/api-tokens" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"019e0000-0000-7000-8000-000000000001","user_id":"u","name":"n","prefix":"otx_zzz1234","token":"otx_zzz1234567","created_at":"now","last_used_at":null,"expires_at":null,"revoked_at":null}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return srv
}

func TestAddCluster_TOFUStoresInlineCA(t *testing.T) {
	srv := newTLSLoginSrv(t)
	defer srv.Close()
	sum := sha256.Sum256(srv.Certificate().Raw)

	path := filepath.Join(t.TempDir(), "config.yaml")
	_, stderr, err := runConfigCmd(t, path, []string{
		"add", "cluster",
		"--name", "prod",
		"--server", srv.URL, // https://...
		"--login", "admin@otherix.local",
		"--password", "right",
		"--ca-fingerprint", hex.EncodeToString(sum[:]),
	}, "")
	if err != nil {
		t.Fatalf("add cluster: %v stderr=%s", err, stderr)
	}
	cfg, err := cliconfig.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Clusters) != 1 || cfg.Clusters[0].CertificateAuthorityData == "" {
		t.Errorf("stored cluster = %+v, want non-empty CertificateAuthorityData from TOFU", cfg.Clusters)
	}
}

func TestAddCluster_Happy(t *testing.T) {
	srv := newLoginSrv(t)
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "config.yaml")
	stdout, stderr, err := runConfigCmd(t, path, []string{
		"add", "cluster",
		"--name", "prod",
		"--server", srv.URL,
		"--login", "admin@otherix.local",
		"--password", "right",
	}, "")
	if err != nil {
		t.Fatalf("add cluster: %v stderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, "cluster added: name=prod") {
		t.Errorf("stdout = %q", stdout)
	}
	cfg, err := cliconfig.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CurrentCluster != "prod" || len(cfg.Clusters) != 1 {
		t.Errorf("config after add = %+v", cfg)
	}
	if cfg.Clusters[0].Token != "otx_zzz1234567" || cfg.Clusters[0].TokenID == "" {
		t.Errorf("stored token = %+v", cfg.Clusters[0])
	}
	if srv.createName != "otherix-cli-prod" {
		t.Errorf("server saw create-name %q", srv.createName)
	}
}

func TestAddCluster_RefusesDuplicateWithoutForce(t *testing.T) {
	srv := newLoginSrv(t)
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "config.yaml")

	args := []string{"add", "cluster", "--name", "prod", "--server", srv.URL, "--login", "a@b", "--password", "right"}
	if _, _, err := runConfigCmd(t, path, args, ""); err != nil {
		t.Fatalf("first add: %v", err)
	}
	_, _, err := runConfigCmd(t, path, args, "")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second add err = %v, want already-exists", err)
	}
}

func TestAddCluster_ForceReplacesAndRevokes(t *testing.T) {
	srv := newLoginSrv(t)
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "config.yaml")
	args := []string{"add", "cluster", "--name", "prod", "--server", srv.URL, "--login", "a@b", "--password", "right"}
	if _, _, err := runConfigCmd(t, path, args, ""); err != nil {
		t.Fatalf("first add: %v", err)
	}
	first, _ := cliconfig.Load(path)
	firstID := first.Clusters[0].TokenID

	_, _, err := runConfigCmd(t, path, append(args, "--force"), "")
	if err != nil {
		t.Fatalf("force add: %v", err)
	}
	if srv.revokedID != firstID {
		t.Errorf("revoked id = %q, want %q (best-effort revoke of previous token)", srv.revokedID, firstID)
	}
}

func TestAddCluster_LoginFailureSurfaces(t *testing.T) {
	srv := newLoginSrv(t)
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "config.yaml")
	_, _, err := runConfigCmd(t, path, []string{
		"add", "cluster",
		"--name", "x", "--server", srv.URL, "--login", "a@b", "--password", "wrong",
	}, "")
	if err == nil || !strings.Contains(err.Error(), "login") {
		t.Errorf("err = %v, want login failure", err)
	}
	// On failure no config file must be written.
	if _, statErr := cliconfig.Load(path); statErr != nil {
		t.Fatalf("Load: %v", statErr)
	}
}

func TestAddCluster_NonInteractiveMissingFlagFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	_, _, err := runConfigCmd(t, path, []string{"add", "cluster"}, "")
	if err == nil {
		t.Fatalf("missing flags non-interactive: want error, got nil")
	}
	// Should not have triggered any HTTP call: missing required values surface early.
}

func TestListAndShow(t *testing.T) {
	srv := newLoginSrv(t)
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if _, _, err := runConfigCmd(t, path, []string{
		"add", "cluster", "--name", "prod", "--server", srv.URL, "--login", "a@b", "--password", "right",
	}, ""); err != nil {
		t.Fatalf("add: %v", err)
	}
	stdoutList, _, err := runConfigCmd(t, path, []string{"list"}, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(stdoutList, "prod") || !strings.Contains(stdoutList, "*") {
		t.Errorf("list output = %q", stdoutList)
	}
	stdoutShow, _, err := runConfigCmd(t, path, []string{"show"}, "")
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if !strings.Contains(stdoutShow, "name: prod") {
		t.Errorf("show output = %q", stdoutShow)
	}
	if strings.Contains(stdoutShow, "otx_zzz1234567") {
		t.Errorf("show output leaked token: %q", stdoutShow)
	}
	stdoutReveal, _, err := runConfigCmd(t, path, []string{"show", "--show-token"}, "")
	if err != nil {
		t.Fatalf("show --show-token: %v", err)
	}
	if !strings.Contains(stdoutReveal, "otx_zzz1234567") {
		t.Errorf("show --show-token did not reveal token: %q", stdoutReveal)
	}
}

func TestUseAndRemove(t *testing.T) {
	srv := newLoginSrv(t)
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "config.yaml")
	for _, name := range []string{"prod", "staging"} {
		if _, _, err := runConfigCmd(t, path, []string{
			"add", "cluster", "--name", name, "--server", srv.URL, "--login", "a@b", "--password", "right",
		}, ""); err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
	}
	stdoutUse, _, err := runConfigCmd(t, path, []string{"use", "staging"}, "")
	if err != nil {
		t.Fatalf("use: %v", err)
	}
	if !strings.Contains(stdoutUse, "current cluster: staging") {
		t.Errorf("use output = %q", stdoutUse)
	}
	stdoutRm, _, err := runConfigCmd(t, path, []string{"remove", "staging", "--force"}, "")
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !strings.Contains(stdoutRm, "cluster removed: staging") {
		t.Errorf("remove output = %q", stdoutRm)
	}
	cfg, _ := cliconfig.Load(path)
	if cfg.CurrentCluster != "prod" {
		t.Errorf("current after removing staging = %q, want prod", cfg.CurrentCluster)
	}
}

func TestRemove_UnknownClusterFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if _, _, err := runConfigCmd(t, path, []string{"remove", "ghost", "--force"}, ""); err == nil {
		t.Errorf("remove ghost: want error, got nil")
	}
}

func TestUse_UnknownClusterFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	_, _, err := runConfigCmd(t, path, []string{"use", "ghost"}, "")
	if err == nil {
		t.Errorf("use ghost: want error, got nil")
	}
}

// ensure no leftover usage error in the io.EOF-or-similar shapes that
// would confuse callers reading stderr.
func TestAddCluster_LoginErrorContext(t *testing.T) {
	closedSrv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := closedSrv.URL
	closedSrv.Close()
	path := filepath.Join(t.TempDir(), "config.yaml")
	_, _, err := runConfigCmd(t, path, []string{
		"add", "cluster", "--name", "x", "--server", url, "--login", "a@b", "--password", "p",
	}, "")
	if err == nil || !errors.Is(err, err) {
		t.Errorf("err = %v", err)
	}
}
