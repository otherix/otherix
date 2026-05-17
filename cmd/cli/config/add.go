// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package config

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cliconfig"
	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// Env-var names mirror the flag names (kubectl-style: каждый flag
// has an env counterpart). The token & endpoint vars match the
// names defined в cliconfig (re-used here for consistency); the
// login & password vars are introduced by this command.
const (
	envLogin    = "OTHERIX_LOGIN"
	envPassword = "OTHERIX_PASSWORD" //nolint:gosec // env var name, not a credential
)

func newAddCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add а cluster credential (currently: `add cluster`)",
	}
	cmd.AddCommand(newAddClusterCommand())
	return cmd
}

func newAddClusterCommand() *cobra.Command {
	var (
		name       string
		server     string
		login      string
		password   string
		setCurrent bool
		force      bool
	)
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Authenticate against а CP, create an API token, store it",
		Long: `add cluster runs а one-time bootstrap:
  1. Logs into the CP с the supplied email + password (JWT).
  2. Calls /v1/users/me/api-tokens с the JWT, creating а long-
     lived otx_* token named "otherix-cli-<cluster>".
  3. Persists (server, token, token-id) into the config file as а
     new cluster entry.

Missing required values trigger interactive prompts when stdin is
а TTY; --force replaces an existing entry с the same name (best-
effort revoking the old API token server-side first).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAddCluster(cmd, addClusterOptions{
				name:       name,
				server:     server,
				login:      login,
				password:   password,
				setCurrent: setCurrent,
				force:      force,
			})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "cluster name (required)")
	cmd.Flags().StringVar(&server, "server", os.Getenv(cliconfig.EnvServer), "CP base URL (env: OTHERIX_SERVER)")
	cmd.Flags().StringVar(&login, "login", os.Getenv(envLogin), "operator email (env: OTHERIX_LOGIN)")
	cmd.Flags().StringVar(&password, "password", os.Getenv(envPassword), "operator password (env: OTHERIX_PASSWORD)")
	cmd.Flags().BoolVar(&setCurrent, "set-current", true, "make this the current cluster (default true for the first cluster)")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing cluster с the same name (revokes the previous token server-side)")
	return cmd
}

type addClusterOptions struct {
	name, server, login, password string
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

	authed, token, err := loginAndIssueToken(cmd.Context(), server, opts)
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

	if err := writeClusterEntry(cmd.Context(), cfg, authed, server, token, opts, stderr); err != nil {
		return err
	}
	if err := cliconfig.Save(path, cfg); err != nil {
		return err
	}

	current := cfg.CurrentCluster == opts.name
	printf(cmd.OutOrStdout(), "cluster added: name=%s server=%s current=%t\n", opts.name, server, current)
	return nil
}

// loginAndIssueToken handles the two-hop bootstrap — login на the
// anonymous client → upgrade К JWT → create API token. Returns
// the authenticated client (handy for а subsequent best-effort
// revoke) и the created token row.
func loginAndIssueToken(ctx context.Context, server string, opts addClusterOptions) (*cpclient.Client, cpclient.APIToken, error) {
	anon, err := cpclient.NewAnonymous(server, cpclient.Options{})
	if err != nil {
		return nil, cpclient.APIToken{}, fmt.Errorf("init client: %v", err)
	}
	login, err := anon.Login(ctx, cpclient.LoginRequest{
		Email:    opts.login,
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

// writeClusterEntry mutates cfg в-place: optionally revoking the
// previous token under --force, replacing the cluster entry, и
// flipping current-cluster when appropriate. The caller persists
// the result.
func writeClusterEntry(ctx context.Context, cfg *cliconfig.Config, authed *cpclient.Client, server string, token cpclient.APIToken, opts addClusterOptions, stderr io.Writer) error {
	if existing, lookupErr := cfg.FindCluster(opts.name); lookupErr == nil {
		if !opts.force {
			return fmt.Errorf("cluster %q already exists: rerun с --force to replace it", opts.name)
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
// fields when stdin is а TTY; в non-TTY context (CI, scripts) а
// missing value is а hard error and the caller must rerun с the
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
		{&opts.login, "Login (email)", "--login is required (or set OTHERIX_LOGIN)", false},
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
		return errors.New("name, login и password must all be non-empty")
	}
	return nil
}

// readField is а small adapter that picks the prompt flavour by
// whether the field is а secret. Keeps fillMissingAddInputs free
// of nested switches over input kind.
func readField(out io.Writer, reader *bufio.Reader, label string, secret bool) (string, error) {
	if secret {
		return promptPassword(out, label)
	}
	return promptInput(out, label, reader)
}

// bestEffortRevoke fires DELETE /v1/users/me/api-tokens/{id} on
// the previous token. Failures are logged К stderr но never block
// the --force path; the operator can clean up manually if revoke
// fails (e.g. the CP is unreachable but they want К replace а
// known-bad token локально).
func bestEffortRevoke(ctx context.Context, c *cpclient.Client, prev *cliconfig.Cluster, stderr io.Writer) {
	if prev == nil || prev.TokenID == "" {
		_, _ = fmt.Fprintf(stderr, "warning: previous cluster %q has no recorded token id; leaving old token active server-side\n", prev.Name)
		return
	}
	if err := c.RevokeAPIToken(ctx, prev.TokenID); err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: revoke previous token %s failed: %v\n", prev.TokenID, err)
	}
}

// printf is а tiny wrapper that swallows the Write error — used
// for routine status output where the only failure mode is а
// closed pipe (caller already gone).
func printf(out io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(out, format, args...)
}
