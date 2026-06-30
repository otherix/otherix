// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package ssh implements the operator-facing `otherix ssh` command tree.
//
// `otherix ssh <vm> [--login L]` is the one-command path into a VM: it
// resolves the active cluster credential, mints a short-lived guest
// certificate via the shared sshconn connector, and execs the system
// `ssh` client with a ProxyCommand that tunnels the SSH bytes through the
// Control Plane's ssh-stream relay. `otherix ssh proxy <vm> <port>` is the
// ProxyCommand primitive that `ssh` invokes under the hood; it splices its
// own stdin/stdout to the relay and is not normally run by hand.
//
// The package owns no transport logic of its own: certificate minting, the
// relay WebSocket, and TLS trust all live in cmd/cli/internal/sshconn. This
// command is the thin operator-UX layer that turns CLI flags into an
// sshconn.Config and assembles the `ssh` argv.
package ssh

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	"github.com/otherix/otherix/cmd/cli/internal/sshconn"
)

// defaultLogin is the guest login used when --login is omitted. It mirrors
// sshconn's own default principal (an empty login certifies "root"), so a
// bare `otherix ssh <vm>` lands on the same account the connector would mint
// a certificate for.
const defaultLogin = "root"

// Seams. These package-level indirections let tests assert the assembled
// `ssh` argv and the proxy stream wiring without minting a real certificate,
// dialing the relay, or exec'ing a real `ssh` binary. Production wires them
// to the shared connector and the system ssh client.
var (
	ensureGuestCert = sshconn.EnsureGuestCert
	proxyConnect    = sshconn.Proxy
	sshExecutor     = runSystemSSH
)

// NewCommand returns the `otherix ssh` command tree: the interactive
// `ssh <vm>` entry point plus the `ssh proxy <vm> <port>` ProxyCommand
// primitive it spawns. A VM literally named "proxy" is unreachable via the
// bare form (cobra routes the word to the subcommand); such a VM can still
// be reached by wiring an ssh_config Host entry that calls the proxy
// primitive directly.
func NewCommand() *cobra.Command {
	var login string

	cmd := &cobra.Command{
		Use:   "ssh <vm-name>",
		Short: "Open an interactive SSH session to a VM through the control plane.",
		Long: `Opens an interactive SSH session to the named VM.

The command mints a short-lived guest certificate from the control plane,
then execs the system 'ssh' client with a ProxyCommand that tunnels the
SSH connection through the control plane's ssh-stream relay. No inbound
network path to the guest is required - the relay rides the same control
plane endpoint and credential the rest of the CLI uses.

The login defaults to 'root'; override it with --login. The guest
certificate is principal-scoped, so one certificate works on any VM in the
cluster that trusts the cluster CA.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSSH(cmd, args[0], login)
		},
	}
	cmd.Flags().StringVar(&login, "login", "", "guest login to connect as (default "+defaultLogin+")")

	cmd.AddCommand(newProxyCommand())
	return cmd
}

// newProxyCommand returns `otherix ssh proxy <vm> <port>`, the ProxyCommand
// primitive an ssh_config block invokes. It is wired into an `ssh`
// invocation as `ProxyCommand otherix ssh proxy %h %p`; ssh substitutes the
// target host and port, then runs this through the user's shell. It splices
// its own stdin/stdout (the ssh client's pipes) to the relay and exits with
// the relay's status.
func newProxyCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "proxy <vm-name> <port>",
		Short: "Relay primitive used as an ssh ProxyCommand (not run directly).",
		Long: `Splices stdin/stdout to a VM's control-plane ssh-stream relay.

This is the body of an ssh ProxyCommand and is normally invoked by the
system ssh client on behalf of 'otherix ssh <vm>', never run by hand. The
<port> argument is the guest port ssh passes via the %p token; the relay
targets the guest sshd directly, so it is accepted for ProxyCommand
compatibility.`,
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProxy(cmd, args[0], args[1])
		},
	}
}

// runSSH resolves the cluster credential, ensures a usable guest
// certificate for login on vmName, assembles the `ssh` argv (identity +
// certificate + ProxyCommand back into this binary), and execs it.
func runSSH(cmd *cobra.Command, vmName, login string) error {
	cfg, err := sshConfigFromFlags(cmd)
	if err != nil {
		return err
	}
	certPath, keyPath, err := ensureGuestCert(cmd.Context(), cfg, vmName, login)
	if err != nil {
		return fmt.Errorf("prepare ssh credential for %s: %v", vmName, err)
	}
	self, err := os.Executable()
	if err != nil || self == "" {
		// Fall back to the command name; ssh resolves it via PATH. A
		// missing self path is unusual but must not block the connect.
		self = "otherix"
	}
	argv := buildSSHArgv(self, vmName, effectiveLogin(login), certPath, keyPath, proxyPassthrough(cmd))
	return sshExecutor(cmd.Context(), argv)
}

// runProxy parses the guest port and relays stdin/stdout through the
// connector. It is the inner half of the ProxyCommand.
func runProxy(cmd *cobra.Command, vmName, portArg string) error {
	port, err := strconv.Atoi(portArg)
	if err != nil {
		return fmt.Errorf("invalid port %q: %v", portArg, err)
	}
	cfg, err := sshConfigFromFlags(cmd)
	if err != nil {
		return err
	}
	return proxyConnect(cmd.Context(), cfg, vmName, port, cmd.InOrStdin(), cmd.OutOrStdout())
}

// sshConfigFromFlags resolves the active (endpoint, token) credential from
// the inherited persistent flags + env + config file and builds the
// sshconn.Config the connector operations consume. TLS trust is left at the
// connector default (system root store): the dev/smoke path speaks plain
// HTTP and a public CP presents a publicly-trusted certificate. Pinning a
// cluster-CA-signed CP leaf is the fingerprint-based path of the standalone
// connector, not the credentialed operator CLI.
func sshConfigFromFlags(cmd *cobra.Command) (sshconn.Config, error) {
	auth, err := cliauth.ResolveAuth(cmd)
	if err != nil {
		return sshconn.Config{}, err
	}
	return sshconn.Config{
		ServerURL:   auth.Endpoint,
		BearerToken: auth.Token,
	}, nil
}

// effectiveLogin returns login, defaulting blank/whitespace input to
// defaultLogin so the `ssh user@host` destination is always concrete.
func effectiveLogin(login string) string {
	if strings.TrimSpace(login) == "" {
		return defaultLogin
	}
	return login
}

// proxyPassthrough collects the cluster-selection persistent flags the
// operator set so the spawned `ssh proxy` subprocess resolves the same
// cluster, endpoint, and token. The token is propagated only when it was
// supplied on the command line (it is already in the operator's process
// table for the parent invocation); env- and config-sourced tokens reach
// the subprocess through the same env and config file.
func proxyPassthrough(cmd *cobra.Command) []string {
	var out []string
	for _, name := range []string{cliauth.FlagConfig, cliauth.FlagCluster, cliauth.FlagEndpoint, cliauth.FlagToken} {
		if f := cmd.Flags().Lookup(name); f != nil && f.Changed {
			out = append(out, "--"+name, f.Value.String())
		}
	}
	return out
}

// buildSSHArgv assembles the system `ssh` argv: the cached identity key and
// guest certificate, a ProxyCommand that re-enters this binary's `ssh proxy`
// relay, and the login@host destination. IdentitiesOnly pins ssh to the
// supplied certificate rather than probing the operator's agent or default
// keys. The argv[0] "ssh" is resolved via PATH by the executor.
func buildSSHArgv(self, vmName, login, certPath, keyPath string, passthrough []string) []string {
	return []string{
		"ssh",
		"-i", keyPath,
		"-o", "CertificateFile=" + certPath,
		"-o", "IdentitiesOnly=yes",
		"-o", "ProxyCommand=" + buildProxyCommand(self, passthrough),
		login + "@" + vmName,
	}
}

// buildProxyCommand renders the ProxyCommand string ssh runs through the
// user's shell. Every fixed token (the binary path and any propagated
// flags) is single-quoted so paths or values with shell metacharacters
// survive; the trailing `%h %p` are ssh substitution tokens and must stay
// unquoted so ssh expands them to the target host and port.
func buildProxyCommand(self string, passthrough []string) string {
	parts := make([]string, 0, len(passthrough)+5)
	parts = append(parts, shellQuote(self))
	for _, p := range passthrough {
		parts = append(parts, shellQuote(p))
	}
	parts = append(parts, "ssh", "proxy", "%h", "%p")
	return strings.Join(parts, " ")
}

// shellQuote wraps s in single quotes, escaping any embedded single quote
// the POSIX way ('\”) so the result is one safe shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// runSystemSSH execs the system ssh client found on PATH, inheriting the
// process's terminal so the operator gets a normal interactive session, and
// returns ssh's exit status as the command error.
func runSystemSSH(ctx context.Context, argv []string) error {
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("%s not found in PATH: %v", argv[0], err)
	}
	c := exec.CommandContext(ctx, bin, argv[1:]...) //nolint:gosec // G204: argv is the operator's own ssh invocation assembled by buildSSHArgv; running the system ssh client with operator-controlled arguments is the command's entire purpose.
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}
