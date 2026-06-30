// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"context"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/otherix/otherix/cmd/cli/sshgrant"
	"github.com/otherix/otherix/cmd/internal/sshconn"
)

// testBundle builds a ca-bundle-trust bundle for the connector to import.
func testBundle(t *testing.T) (sshgrant.Bundle, string) {
	t.Helper()
	caPEM := "-----BEGIN CERTIFICATE-----\nMIIBdummy\n-----END CERTIFICATE-----\n"
	b := sshgrant.Bundle{
		Version:   sshgrant.BundleVersion,
		ServerURL: "https://cp.example.com:8443",
		Trust:     sshgrant.TrustCABundle,
		// ResolveTrust base64-decodes CACertPEM, so it must be base64 of the PEM.
		CACertPEM: base64.StdEncoding.EncodeToString([]byte(caPEM)),
		Token:     "otx_sshgrant_secrettoken",
		VMs: []sshgrant.BundleVM{
			{VM: "web01", Login: "root"},
			{VM: "db01", Login: "ubuntu"},
		},
	}
	blob, err := sshgrant.EncodeBundle(b)
	if err != nil {
		t.Fatalf("EncodeBundle() error = %v", err)
	}
	return b, blob
}

// runCmd executes the root command with args and returns its error.
func runCmd(t *testing.T, args ...string) error {
	t.Helper()
	root := newRootCmd()
	root.SetArgs(args)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	return root.Execute()
}

func TestAddWritesStateAndConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_, blob := testBundle(t)

	bundleFile := filepath.Join(home, "grant.txt")
	if err := os.WriteFile(bundleFile, []byte(blob), 0o600); err != nil {
		t.Fatalf("write bundle file: %v", err)
	}

	if err := runCmd(t, "add", bundleFile); err != nil {
		t.Fatalf("add error = %v", err)
	}

	dir := filepath.Join(home, ".otherix", "ssh")

	// State file exists with 0600 perms.
	statePath := filepath.Join(dir, "state.json")
	info, err := os.Stat(statePath)
	if err != nil {
		t.Fatalf("stat state file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("state file perm = %o, want %o", got, 0o600)
	}

	st, err := loadState(dir)
	if err != nil {
		t.Fatalf("loadState() error = %v", err)
	}
	if st.ServerURL != "https://cp.example.com:8443" {
		t.Errorf("state ServerURL = %q, want %q", st.ServerURL, "https://cp.example.com:8443")
	}
	if st.Token != "otx_sshgrant_secrettoken" {
		t.Errorf("state Token = %q, want %q", st.Token, "otx_sshgrant_secrettoken")
	}
	if st.ClusterSuffix != "otherix" {
		t.Errorf("state ClusterSuffix = %q, want %q", st.ClusterSuffix, "otherix")
	}

	// Managed ssh_config fragment carries the wildcard ProxyCommand block.
	managed, err := os.ReadFile(filepath.Join(dir, "config"))
	if err != nil {
		t.Fatalf("read managed config: %v", err)
	}
	if !strings.Contains(string(managed), "Host *.otherix") {
		t.Errorf("managed config missing wildcard host block:\n%s", managed)
	}
	if !strings.Contains(string(managed), "proxy %h %p") {
		t.Errorf("managed config missing ProxyCommand:\n%s", managed)
	}

	// ~/.ssh/config has the Include line wiring the managed fragment.
	userCfg, err := os.ReadFile(filepath.Join(home, ".ssh", "config"))
	if err != nil {
		t.Fatalf("read ~/.ssh/config: %v", err)
	}
	wantInclude := "Include " + filepath.Join(dir, "config")
	if !strings.Contains(string(userCfg), wantInclude) {
		t.Errorf("~/.ssh/config missing %q:\n%s", wantInclude, userCfg)
	}
}

func TestAddIsIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_, blob := testBundle(t)

	if err := runCmd(t, "add", blob); err != nil {
		t.Fatalf("first add error = %v", err)
	}
	if err := runCmd(t, "add", blob); err != nil {
		t.Fatalf("second add error = %v", err)
	}

	dir := filepath.Join(home, ".otherix", "ssh")
	wantInclude := "Include " + filepath.Join(dir, "config")
	userCfg, err := os.ReadFile(filepath.Join(home, ".ssh", "config"))
	if err != nil {
		t.Fatalf("read ~/.ssh/config: %v", err)
	}
	if n := strings.Count(string(userCfg), wantInclude); n != 1 {
		t.Errorf("Include line count = %d, want 1:\n%s", n, userCfg)
	}

	managed, err := os.ReadFile(filepath.Join(dir, "config"))
	if err != nil {
		t.Fatalf("read managed config: %v", err)
	}
	if n := strings.Count(string(managed), "Host *.otherix"); n != 1 {
		t.Errorf("wildcard host block count = %d, want 1:\n%s", n, managed)
	}
}

func TestAddPreservesExistingUserConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatalf("mkdir .ssh: %v", err)
	}
	existing := "Host bastion\n    HostName 10.0.0.1\n"
	if err := os.WriteFile(filepath.Join(home, ".ssh", "config"), []byte(existing), 0o600); err != nil {
		t.Fatalf("seed ~/.ssh/config: %v", err)
	}
	_, blob := testBundle(t)
	if err := runCmd(t, "add", blob); err != nil {
		t.Fatalf("add error = %v", err)
	}
	userCfg, err := os.ReadFile(filepath.Join(home, ".ssh", "config"))
	if err != nil {
		t.Fatalf("read ~/.ssh/config: %v", err)
	}
	if !strings.Contains(string(userCfg), "Host bastion") {
		t.Errorf("existing user config was clobbered:\n%s", userCfg)
	}
}

func TestTrustRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_, blob := testBundle(t)
	if err := runCmd(t, "add", blob); err != nil {
		t.Fatalf("add error = %v", err)
	}
	dir := filepath.Join(home, ".otherix", "ssh")
	st, err := loadState(dir)
	if err != nil {
		t.Fatalf("loadState() error = %v", err)
	}
	cfg := sshConfigFromState(st, dir)
	if cfg.ServerURL != "https://cp.example.com:8443" {
		t.Errorf("cfg.ServerURL = %q, want %q", cfg.ServerURL, "https://cp.example.com:8443")
	}
	if cfg.BearerToken != "otx_sshgrant_secrettoken" {
		t.Errorf("cfg.BearerToken = %q", cfg.BearerToken)
	}
	if !strings.Contains(string(cfg.CACertPEM), "BEGIN CERTIFICATE") {
		t.Errorf("cfg.CACertPEM did not round-trip: %q", cfg.CACertPEM)
	}
	if cfg.KnownDir != dir {
		t.Errorf("cfg.KnownDir = %q, want %q", cfg.KnownDir, dir)
	}
}

func TestProxyLoadsStateAndCallsSeam(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_, blob := testBundle(t)
	if err := runCmd(t, "add", blob); err != nil {
		t.Fatalf("add error = %v", err)
	}

	var gotCfg sshconn.Config
	var gotVM string
	var gotPort int
	orig := proxyConnect
	t.Cleanup(func() { proxyConnect = orig })
	proxyConnect = func(_ context.Context, cfg sshconn.Config, vmName string, port int, _ io.Reader, _ io.Writer) error {
		gotCfg = cfg
		gotVM = vmName
		gotPort = port
		return nil
	}

	// ssh hands the ProxyCommand the full hostname (vm + suffix) as %h.
	if err := runCmd(t, "proxy", "web01.otherix", "22"); err != nil {
		t.Fatalf("proxy error = %v", err)
	}
	if gotVM != "web01" {
		t.Errorf("proxy vm = %q, want %q", gotVM, "web01")
	}
	if gotPort != 22 {
		t.Errorf("proxy port = %d, want 22", gotPort)
	}
	if gotCfg.ServerURL != "https://cp.example.com:8443" {
		t.Errorf("proxy cfg.ServerURL = %q", gotCfg.ServerURL)
	}
	if gotCfg.BearerToken != "otx_sshgrant_secrettoken" {
		t.Errorf("proxy cfg.BearerToken = %q", gotCfg.BearerToken)
	}
}

func TestAddBadBundleNoPartialState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := runCmd(t, "add", "this-is-not-a-bundle"); err == nil {
		t.Fatalf("add with garbage bundle = nil error, want non-nil")
	}
	statePath := filepath.Join(home, ".otherix", "ssh", "state.json")
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("state file should not exist after bad bundle, stat err = %v", err)
	}
}
