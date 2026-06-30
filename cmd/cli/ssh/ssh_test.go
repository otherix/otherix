// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package ssh

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	"github.com/otherix/otherix/cmd/internal/sshconn"
)

func TestEffectiveLogin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"", "root"},
		{"alice", "alice"},
		{"  ", "root"},
	}
	for _, c := range cases {
		if got := effectiveLogin(c.in); got != c.want {
			t.Errorf("effectiveLogin(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBuildProxyCommand(t *testing.T) {
	t.Parallel()
	got := buildProxyCommand("/usr/bin/otherix", []string{"--cluster", "prod"})
	want := `'/usr/bin/otherix' '--cluster' 'prod' ssh proxy %h %p`
	if got != want {
		t.Errorf("buildProxyCommand = %q, want %q", got, want)
	}
}

func TestBuildSSHArgv(t *testing.T) {
	t.Parallel()
	got := buildSSHArgv("/usr/bin/otherix", "db", "root", "/c/cert", "/k/key", nil)
	want := []string{
		"ssh",
		"-i", "/k/key",
		"-o", "CertificateFile=/c/cert",
		"-o", "IdentitiesOnly=yes",
		"-o", "UserKnownHostsFile=/c/known_hosts",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ProxyCommand='/usr/bin/otherix' ssh proxy %h %p",
		"root@db",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("buildSSHArgv mismatch (-want +got):\n%s", diff)
	}
}

// newSSHRunner returns a helper that drives the ssh command tree with the
// root persistent flags an actual invocation inherits, so the command
// resolves auth exactly as it would under the real root command.
func newSSHRunner(t *testing.T) (run func(args []string, stdin io.Reader) (string, string, error)) {
	t.Helper()
	return func(args []string, stdin io.Reader) (string, string, error) {
		parent := NewCommand()
		parent.PersistentFlags().String(cliauth.FlagConfig, "", "")
		parent.PersistentFlags().String(cliauth.FlagEndpoint, "", "")
		parent.PersistentFlags().String(cliauth.FlagToken, "", "")
		parent.PersistentFlags().String(cliauth.FlagCluster, "", "")
		parent.SetArgs(args)
		parent.SilenceUsage = true
		parent.SilenceErrors = true
		var out, errBuf bytes.Buffer
		parent.SetOut(&out)
		parent.SetErr(&errBuf)
		if stdin != nil {
			parent.SetIn(stdin)
		}
		parent.SetContext(context.Background())
		err := parent.Execute()
		return out.String(), errBuf.String(), err
	}
}

func TestProxyWiringBuildsConfigAndSplicesStreams(t *testing.T) {
	var gotCfg sshconn.Config
	var gotVM string
	var gotPort int
	var gotStdin string

	origProxy := proxyConnect
	t.Cleanup(func() { proxyConnect = origProxy })
	proxyConnect = func(_ context.Context, cfg sshconn.Config, vmName string, port int, stdin io.Reader, stdout io.Writer) error {
		gotCfg = cfg
		gotVM = vmName
		gotPort = port
		b, _ := io.ReadAll(stdin)
		gotStdin = string(b)
		_, _ = io.WriteString(stdout, "OK")
		return nil
	}

	run := newSSHRunner(t)
	out, _, err := run([]string{
		"--endpoint", "https://cp.example:8443", "--token", "tok-123",
		"proxy", "myvm", "22",
	}, strings.NewReader("client-bytes"))
	if err != nil {
		t.Fatalf("proxy run error: %v", err)
	}

	if gotCfg.ServerURL != "https://cp.example:8443" {
		t.Errorf("cfg.ServerURL = %q, want %q", gotCfg.ServerURL, "https://cp.example:8443")
	}
	if gotCfg.BearerToken != "tok-123" {
		t.Errorf("cfg.BearerToken = %q, want %q", gotCfg.BearerToken, "tok-123")
	}
	if gotVM != "myvm" {
		t.Errorf("vm = %q, want myvm", gotVM)
	}
	if gotPort != 22 {
		t.Errorf("port = %d, want 22", gotPort)
	}
	if gotStdin != "client-bytes" {
		t.Errorf("proxy stdin = %q, want %q", gotStdin, "client-bytes")
	}
	if !strings.Contains(out, "OK") {
		t.Errorf("proxy stdout = %q, want it to carry the relay's bytes", out)
	}
}

func TestSSHWiringAssemblesArgv(t *testing.T) {
	var gotArgv []string

	origEnsure := ensureGuestCert
	origExec := sshExecutor
	t.Cleanup(func() {
		ensureGuestCert = origEnsure
		sshExecutor = origExec
	})
	ensureGuestCert = func(_ context.Context, _ sshconn.Config, _, _ string) (string, string, error) {
		return "/fake/cert", "/fake/key", nil
	}
	sshExecutor = func(_ context.Context, argv, _ []string) error {
		gotArgv = argv
		return nil
	}

	run := newSSHRunner(t)
	_, _, err := run([]string{
		"--endpoint", "https://cp.example:8443", "--token", "tok-123",
		"myvm", "--login", "alice",
	}, nil)
	if err != nil {
		t.Fatalf("ssh run error: %v", err)
	}

	joined := strings.Join(gotArgv, " ")
	for _, want := range []string{
		"ssh", "-i /fake/key", "CertificateFile=/fake/cert", "alice@myvm",
		"ssh proxy %h %p",
		"-o UserKnownHostsFile=/fake/known_hosts",
		"-o StrictHostKeyChecking=accept-new",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("assembled argv %q missing %q", joined, want)
		}
	}
	// The ProxyCommand must propagate the selected endpoint so the
	// spawned `ssh proxy` subprocess resolves the same cluster.
	if !strings.Contains(joined, "https://cp.example:8443") {
		t.Errorf("assembled argv %q does not propagate the endpoint into ProxyCommand", joined)
	}
}

// TestSSHTokenPassedViaEnvNotArgv asserts a --token supplied on the command
// line is injected into the spawned ssh process environment (where the child
// `ssh proxy` reads it via OTHERIX_API_TOKEN) and never embedded in the
// ProxyCommand argv, which `ps` would expose to other local users.
func TestSSHTokenPassedViaEnvNotArgv(t *testing.T) {
	var gotArgv, gotEnv []string

	origEnsure := ensureGuestCert
	origExec := sshExecutor
	t.Cleanup(func() {
		ensureGuestCert = origEnsure
		sshExecutor = origExec
	})
	ensureGuestCert = func(_ context.Context, _ sshconn.Config, _, _ string) (string, string, error) {
		return "/fake/cert", "/fake/key", nil
	}
	sshExecutor = func(_ context.Context, argv, env []string) error {
		gotArgv = argv
		gotEnv = env
		return nil
	}

	run := newSSHRunner(t)
	_, _, err := run([]string{
		"--endpoint", "https://cp.example:8443", "--token", "secret-tok",
		"myvm",
	}, nil)
	if err != nil {
		t.Fatalf("ssh run error: %v", err)
	}

	if joined := strings.Join(gotArgv, " "); strings.Contains(joined, "secret-tok") {
		t.Errorf("token leaked into ssh argv: %q", joined)
	}
	want := "OTHERIX_API_TOKEN=secret-tok"
	found := false
	for _, e := range gotEnv {
		if e == want {
			found = true
		}
	}
	if !found {
		t.Errorf("token env %q not injected into ssh process env: %v", want, gotEnv)
	}
}

// TestSSHWhitespaceLoginIsConsistent guards the normalize-once fix: a
// whitespace --login must mint a cert for the same principal the ssh
// destination targets (root), never a cert for the raw whitespace string.
func TestSSHWhitespaceLoginIsConsistent(t *testing.T) {
	var certLogin string
	var gotArgv []string

	origEnsure := ensureGuestCert
	origExec := sshExecutor
	t.Cleanup(func() {
		ensureGuestCert = origEnsure
		sshExecutor = origExec
	})
	ensureGuestCert = func(_ context.Context, _ sshconn.Config, _, login string) (string, string, error) {
		certLogin = login
		return "/fake/cert", "/fake/key", nil
	}
	sshExecutor = func(_ context.Context, argv, _ []string) error {
		gotArgv = argv
		return nil
	}

	run := newSSHRunner(t)
	_, _, err := run([]string{
		"--endpoint", "https://cp.example:8443", "--token", "tok-123",
		"myvm", "--login", "  ",
	}, nil)
	if err != nil {
		t.Fatalf("ssh run error: %v", err)
	}

	if certLogin != "root" {
		t.Errorf("cert minted for login %q, want root", certLogin)
	}
	if joined := strings.Join(gotArgv, " "); !strings.Contains(joined, "root@myvm") {
		t.Errorf("ssh destination in argv %q, want root@myvm", joined)
	}
}
