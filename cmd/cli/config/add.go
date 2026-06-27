// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package config

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/catrust"
	"github.com/otherix/otherix/cmd/cli/internal/cliconfig"
	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// Env-var names mirror the flag names (kubectl-style: each flag
// has an env counterpart). The token & endpoint vars match the
// names defined in cliconfig (re-used here for consistency); the
// login & password vars are introduced by this command.
const (
	envLogin    = "OTHERIX_LOGIN"
	envPassword = "OTHERIX_PASSWORD" //nolint:gosec // env var name, not a credential
)

func newAddCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add a cluster credential (currently: `add cluster`)",
	}
	cmd.AddCommand(newAddClusterCommand())
	return cmd
}

func newAddClusterCommand() *cobra.Command {
	var (
		name          string
		server        string
		login         string
		password      string
		caFile        string
		caData        string
		caFingerprint string
		insecureSkip  bool
		setCurrent    bool
		force         bool
	)
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Authenticate against a CP, create an API token, store it",
		Long: `add cluster runs a one-time bootstrap:
  1. Logs into the CP with the supplied username + password (JWT).
  2. Calls /v1/users/me/api-tokens with the JWT, creating a long-
     lived otx_* token named "otherix-cli-<cluster>".
  3. Persists (server, token, token-id) into the config file as a
     new cluster entry.

Missing required values trigger interactive prompts when stdin is
a TTY; --force replaces an existing entry with the same name (best-
effort revoking the old API token server-side first).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAddCluster(cmd, addClusterOptions{
				name:          name,
				server:        server,
				login:         login,
				password:      password,
				caFile:        caFile,
				caData:        caData,
				caFingerprint: caFingerprint,
				insecureSkip:  insecureSkip,
				setCurrent:    setCurrent,
				force:         force,
			})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "cluster name (required)")
	cmd.Flags().StringVar(&server, "server", os.Getenv(cliconfig.EnvServer), "CP base URL (env: OTHERIX_SERVER)")
	cmd.Flags().StringVar(&login, "login", os.Getenv(envLogin), "operator username (env: OTHERIX_LOGIN)")
	cmd.Flags().StringVar(&password, "password", os.Getenv(envPassword), "operator password (env: OTHERIX_PASSWORD)")
	cmd.Flags().BoolVar(&setCurrent, "set-current", true, "make this the current cluster (default true for the first cluster)")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing cluster with the same name (revokes the previous token server-side)")
	cmd.Flags().StringVar(&caFile, "ca-file", "", "PEM file to trust for this cluster's HTTPS endpoint")
	cmd.Flags().StringVar(&caData, "ca-data", "", "base64 PEM bundle to trust (alternative to --ca-file)")
	cmd.Flags().StringVar(&caFingerprint, "ca-fingerprint", "", "expected sha256 fingerprint (hex) of the cluster CA; verifies the TOFU fetch non-interactively")
	cmd.Flags().BoolVar(&insecureSkip, "insecure-skip-tls-verify", false, "do not verify the CP TLS certificate (development only)")
	return cmd
}

type addClusterOptions struct {
	name, server, login, password string
	caFile, caData, caFingerprint string
	insecureSkip                  bool
	setCurrent, force             bool
}

func runAddCluster(cmd *cobra.Command, opts addClusterOptions) error {
	stderr := cmd.ErrOrStderr()
	reader := bufio.NewReader(cmd.InOrStdin())

	if err := fillMissingAddInputs(stderr, reader, &opts); err != nil {
		return err
	}
	server := strings.TrimRight(opts.server, "/")
	if server == "" {
		return errors.New("server URL is required")
	}

	caPEM, insecure, err := acquireTrust(cmd, server, opts)
	if err != nil {
		return err
	}

	authed, token, err := loginAndIssueToken(cmd.Context(), server, opts, caPEM, insecure)
	if err != nil {
		return err
	}

	path, err := resolveConfigPath(cmd)
	if err != nil {
		return err
	}
	cfg, err := cliconfig.Load(path)
	if err != nil {
		return err
	}

	if err := writeClusterEntry(cmd.Context(), cfg, authed, server, token, opts, caPEM, insecure, stderr); err != nil {
		return err
	}
	if err := cliconfig.Save(path, cfg); err != nil {
		return err
	}

	current := cfg.CurrentCluster == opts.name
	printf(cmd.OutOrStdout(), "cluster added: name=%s server=%s current=%t\n", opts.name, server, current)
	return nil
}

// loginAndIssueToken handles the two-hop bootstrap — login on the
// anonymous client → upgrade to JWT → create API token. Returns
// the authenticated client (handy for a subsequent best-effort
// revoke) and the created token row.
func loginAndIssueToken(ctx context.Context, server string, opts addClusterOptions, caPEM []byte, insecure bool) (*cpclient.Client, cpclient.APIToken, error) {
	anon, err := cpclient.NewAnonymous(server, cpclient.Options{CACertPEM: caPEM, InsecureSkipVerify: insecure})
	if err != nil {
		return nil, cpclient.APIToken{}, fmt.Errorf("init client: %v", err)
	}
	login, err := anon.Login(ctx, cpclient.LoginRequest{
		Username: opts.login,
		Password: opts.password,
	})
	if err != nil {
		return nil, cpclient.APIToken{}, fmt.Errorf("login: %v", err)
	}
	authed := anon.WithToken(login.AccessToken)
	token, err := authed.CreateAPIToken(ctx, cpclient.CreateAPITokenRequest{
		Name: "otherix-cli-" + opts.name,
	})
	if err != nil {
		return nil, cpclient.APIToken{}, fmt.Errorf("create api token: %v", err)
	}
	return authed, token, nil
}

// acquireTrust establishes the TLS trust the CLI will use for server.
// For a plaintext (http) endpoint it is a no-op. For https it applies,
// in order: explicit --ca-file/--ca-data, --insecure-skip-tls-verify,
// a system-trust probe, then a TOFU fetch+pin of /v1/ca. It returns
// the PEM to embed (nil = system trust) and whether to skip verify.
func acquireTrust(cmd *cobra.Command, server string, opts addClusterOptions) (caPEM []byte, insecure bool, err error) {
	if !strings.HasPrefix(server, "https://") {
		return nil, false, nil
	}
	switch {
	case opts.caFile != "" || opts.caData != "":
		p, perr := readExplicitCA(opts.caFile, opts.caData)
		return p, false, perr
	case opts.insecureSkip:
		return nil, true, nil
	}
	if systemTrustOK(cmd.Context(), server) {
		return nil, false, nil
	}
	confirm := func(fp string) (bool, error) {
		return promptTrust(cmd.ErrOrStderr(), cmd.InOrStdin(), server, fp)
	}
	p, ferr := catrust.FetchAndPin(cmd.Context(), server, opts.caFingerprint, confirm)
	if ferr != nil {
		return nil, false, ferr
	}
	return p, false, nil
}

// readExplicitCA loads PEM from --ca-file or decodes --ca-data.
func readExplicitCA(file, data string) ([]byte, error) {
	if file != "" {
		raw, err := os.ReadFile(file) //nolint:gosec // operator-supplied trust path
		if err != nil {
			return nil, fmt.Errorf("read --ca-file: %v", err)
		}
		return raw, nil
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, fmt.Errorf("decode --ca-data: %v", err)
	}
	return raw, nil
}

// systemTrustOK probes server with the system trust store. It returns
// true when the TLS handshake verifies; false on any error (untrusted
// CA, hostname mismatch, connection refused) so the caller falls back to
// TOFU, which surfaces a genuine connection failure itself.
func systemTrustOK(ctx context.Context, server string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(server, "/")+"/v1/ca", nil)
	if err != nil {
		return false
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}

// promptTrust shows the fetched fingerprint and asks the operator to
// confirm on a TTY. Non-TTY without --ca-fingerprint reaches here only
// when FetchAndPin was given an empty fingerprint; return an error so
// the caller fails closed.
func promptTrust(stderr io.Writer, stdin io.Reader, server, fingerprint string) (bool, error) {
	if !stdinIsTerminal() {
		return false, fmt.Errorf("cannot verify CA for %s non-interactively: pass --ca-fingerprint %s (verify it out of band) or --ca-file", server, fingerprint)
	}
	_, _ = fmt.Fprintf(stderr, "CP %s presented a CA with fingerprint:\n  sha256:%s\nTrust this CA? [y/N]: ", server, fingerprint)
	line, _ := bufio.NewReader(stdin).ReadString('\n')
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes", nil
}

// writeClusterEntry mutates cfg in-place: optionally revoking the
// previous token under --force, replacing the cluster entry, and
// flipping current-cluster when appropriate. The caller persists
// the result.
func writeClusterEntry(ctx context.Context, cfg *cliconfig.Config, authed *cpclient.Client, server string, token cpclient.APIToken, opts addClusterOptions, caPEM []byte, insecure bool, stderr io.Writer) error {
	if existing, lookupErr := cfg.FindCluster(opts.name); lookupErr == nil {
		if !opts.force {
			return fmt.Errorf("cluster %q already exists: rerun with --force to replace it", opts.name)
		}
		bestEffortRevoke(ctx, authed, existing, stderr)
		if err := cfg.RemoveCluster(opts.name); err != nil {
			return fmt.Errorf("remove existing cluster: %v", err)
		}
	}
	entry := cliconfig.Cluster{
		Name:    opts.name,
		Server:  server,
		Token:   token.Token,
		TokenID: token.ID,
	}
	if len(caPEM) > 0 {
		entry.CertificateAuthorityData = base64.StdEncoding.EncodeToString(caPEM)
	}
	entry.InsecureSkipTLSVerify = insecure
	if err := cfg.AddCluster(entry); err != nil {
		return err
	}
	if opts.setCurrent || cfg.CurrentCluster == "" {
		if err := cfg.SetCurrent(opts.name); err != nil {
			return err
		}
	}
	return nil
}

// fillMissingAddInputs prompts interactively for missing required
// fields when stdin is a TTY; in non-TTY context (CI, scripts) a
// missing value is a hard error and the caller must rerun with the
// missing flag set explicitly.
func fillMissingAddInputs(stderr io.Writer, reader *bufio.Reader, opts *addClusterOptions) error {
	tty := stdinIsTerminal()
	fields := []struct {
		target  *string
		label   string
		flagMsg string
		secret  bool
	}{
		{&opts.name, "Cluster name", "--name is required", false},
		{&opts.server, "Server URL", "--server is required (or set OTHERIX_SERVER)", false},
		{&opts.login, "Login (username)", "--login is required (or set OTHERIX_LOGIN)", false},
		{&opts.password, "Password", "--password is required (or set OTHERIX_PASSWORD)", true},
	}
	for _, f := range fields {
		if *f.target != "" {
			continue
		}
		if !tty {
			return errors.New(f.flagMsg)
		}
		v, err := readField(stderr, reader, f.label, f.secret)
		if err != nil {
			return err
		}
		*f.target = v
	}
	if opts.name == "" || opts.login == "" || opts.password == "" {
		return errors.New("name, login and password must all be non-empty")
	}
	return nil
}

// readField is a small adapter that picks the prompt flavour by
// whether the field is a secret. Keeps fillMissingAddInputs free
// of nested switches over input kind.
func readField(out io.Writer, reader *bufio.Reader, label string, secret bool) (string, error) {
	if secret {
		return promptPassword(out, label)
	}
	return promptInput(out, label, reader)
}

// bestEffortRevoke fires DELETE /v1/users/me/api-tokens/{id} on
// the previous token. Failures are logged to stderr but never block
// the --force path; the operator can clean up manually if revoke
// fails (e.g. the CP is unreachable but they want to replace a
// known-bad token locally).
func bestEffortRevoke(ctx context.Context, c *cpclient.Client, prev *cliconfig.Cluster, stderr io.Writer) {
	if prev == nil || prev.TokenID == "" {
		_, _ = fmt.Fprintf(stderr, "warning: previous cluster %q has no recorded token id; leaving old token active server-side\n", prev.Name)
		return
	}
	if err := c.RevokeAPIToken(ctx, prev.TokenID); err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: revoke previous token %s failed: %v\n", prev.TokenID, err)
	}
}

// printf is a tiny wrapper that swallows the Write error — used
// for routine status output where the only failure mode is a
// closed pipe (caller already gone).
func printf(out io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(out, format, args...)
}
