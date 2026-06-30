// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Command otherix-ssh is the thin SSH-only connector an external person
// installs to reach a granted VM through the Otherix control plane. It needs
// no Otherix account and no management CLI: an operator runs `otherix
// ssh-grant create` and sends back a single paste-able bundle, the external
// runs `otherix-ssh add <bundle>` once, and from then on `ssh <vm>.<suffix>`
// tunnels straight to the guest sshd over the control plane's ssh-stream
// relay.
//
// Two subcommands:
//
//   - `otherix-ssh add <bundle-file|->` imports the operator's grant bundle:
//     it stores the control-plane URL, TLS-trust material, and grant token to
//     a 0600 state file under ~/.otherix/ssh, writes a managed wildcard
//     ssh_config fragment, and wires that fragment into ~/.ssh/config with an
//     Include line (added once, never clobbering existing content).
//   - `otherix-ssh proxy <vm> <port>` is the ProxyCommand the ssh_config
//     fragment invokes; it rebuilds the connection from the stored state and
//     splices stdin/stdout to the relay. It is not run by hand.
//
// All transport (cert minting, the relay WebSocket, TLS trust) lives in the
// shared cmd/internal/sshconn connector; this binary is the import-and-store
// UX layer. The grant token is a bearer secret: it is persisted 0600 and
// never logged.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/sshgrant"
	"github.com/otherix/otherix/cmd/internal/sshconn"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// defaultSuffix is the cluster DNS-like suffix the external uses to address
// VMs as `<vm>.<suffix>`. It is a synthetic label, not a real domain, so the
// wildcard `Host *.<suffix>` ssh_config rule never shadows a real public host.
// The bundle carries no suffix, so it is operator/external choice via --suffix.
const defaultSuffix = "otherix"

// stateDirParts is the connector's per-user state directory under the home
// dir; it doubles as the sshconn KnownDir (cached keypair, guest cert, managed
// ssh_config fragment) so all connector artifacts live in one place.
var stateDirParts = []string{".otherix", "ssh"}

// stateFileName is the 0600 JSON file holding everything `proxy` needs to
// rebuild the connection without the bundle: the CP URL, TLS-trust material,
// the grant token (a bearer secret), and the cluster suffix.
const stateFileName = "state.json"

// managedConfigName is the managed ssh_config fragment sshconn writes inside
// the state dir; it is pulled into ~/.ssh/config via an Include line.
const managedConfigName = "config"

// includeMarker comments the Include line `add` writes into ~/.ssh/config so a
// human can see where it came from. Idempotency keys off the Include path, not
// this marker.
const includeMarker = "# Added by otherix-ssh"

// proxyConnect is the relay seam. Production wires it to sshconn.Proxy; tests
// override it to assert the resolved config without dialing the network.
var proxyConnect = sshconn.Proxy

// ensureCert is the guest-cert mint seam. Production wires it to
// sshconn.EnsureGuestCert; tests override it to assert the mint inputs without
// a real Control Plane round-trip. The connector mints the guest certificate
// here, inside the ProxyCommand, because `add` imports a grant bundle with no
// network access and so cannot mint at import time. The certificate is a
// short-lived connect-time credential the grant token authorizes; minting it
// just before the relay carries the session guarantees the CertificateFile the
// managed ssh_config references exists by the time the ssh client reads it
// during user authentication (which happens after the ProxyCommand connects).
// A login is deliberately not passed: the Control Plane pins the grant's own
// login, so the minted certificate always certifies exactly the principal the
// grant allows.
var ensureCert = sshconn.EnsureGuestCert

// connectorState is the on-disk shape persisted by `add` and reloaded by
// `proxy`. The CACertPEM bytes marshal to base64 in JSON automatically. Token
// is a bearer secret and is never logged.
type connectorState struct {
	ServerURL             string `json:"server_url"`
	CACertPEM             []byte `json:"ca_cert_pem,omitempty"`
	CAFingerprint         string `json:"ca_fingerprint,omitempty"`
	InsecureSkipTLSVerify bool   `json:"insecure_skip_tls_verify,omitempty"`
	Token                 string `json:"token"`
	ClusterSuffix         string `json:"cluster_suffix"`
}

// newRootCmd assembles the `otherix-ssh` command tree: `add` and `proxy`.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "otherix-ssh",
		Short:         "Thin external SSH connector for Otherix-managed VMs.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newAddCommand(), newProxyCommand())
	return root
}

// newAddCommand returns `otherix-ssh add <bundle-file|->`, the one-time import
// of an operator grant bundle.
func newAddCommand() *cobra.Command {
	var suffix string
	cmd := &cobra.Command{
		Use:   "add <bundle-file|->",
		Short: "Import an operator grant bundle and wire up ssh access.",
		Long: `Imports the grant bundle an operator sent you.

The argument is a path to the bundle file, '-' to read the bundle from
stdin, or the bundle string itself. add stores the control-plane
coordinates and grant token (0600), writes a managed ssh_config fragment,
and adds an Include line to ~/.ssh/config so that 'ssh <vm>.<suffix>'
connects through the control plane. Re-running add is safe and idempotent.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAdd(cmd, args[0], suffix)
		},
	}
	cmd.Flags().StringVar(&suffix, "suffix", defaultSuffix,
		"cluster suffix used to address VMs as <vm>.<suffix>")
	return cmd
}

// newProxyCommand returns `otherix-ssh proxy <vm> <port>`, the ProxyCommand
// primitive the managed ssh_config fragment invokes.
func newProxyCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "proxy <vm> <port>",
		Short: "Relay primitive used as an ssh ProxyCommand (not run directly).",
		Long: `Splices stdin/stdout to a VM's control-plane ssh-stream relay.

This is the body of an ssh ProxyCommand wired up by 'otherix-ssh add'. ssh
passes the full hostname as <vm> (e.g. web01.otherix); the stored cluster
suffix is stripped to recover the VM name. It is not normally run by hand.`,
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProxy(cmd, args[0], args[1])
		},
	}
}

// runAdd parses the bundle, persists the connector state, writes the managed
// ssh_config fragment, and wires the Include into ~/.ssh/config. The bundle is
// fully parsed before anything is written, so a malformed bundle leaves no
// partial state on disk.
func runAdd(cmd *cobra.Command, bundleArg, suffix string) error {
	suffix = strings.TrimSpace(suffix)
	if suffix == "" {
		return errors.New("cluster suffix must not be empty")
	}

	raw, err := readBundle(bundleArg, cmd.InOrStdin())
	if err != nil {
		return err
	}
	bundle, err := sshgrant.ParseBundle(raw)
	if err != nil {
		return fmt.Errorf("parse bundle: %v", err)
	}
	caPEM, fingerprint, insecure, err := bundle.ResolveTrust()
	if err != nil {
		return fmt.Errorf("resolve bundle trust: %v", err)
	}

	dir, err := stateDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state dir: %v", err)
	}

	st := connectorState{
		ServerURL:             bundle.ServerURL,
		CACertPEM:             caPEM,
		CAFingerprint:         fingerprint,
		InsecureSkipTLSVerify: insecure,
		Token:                 bundle.Token,
		ClusterSuffix:         suffix,
	}
	if err := writeState(dir, st); err != nil {
		return err
	}

	self, err := os.Executable()
	if err != nil || self == "" {
		// Fall back to the command name; ssh resolves it via PATH. A missing
		// self path is unusual and must not block the import.
		self = "otherix-ssh"
	}
	cfg := sshConfigFromState(st, dir)
	if err := sshconn.WriteSSHConfigBlock(cfg, suffix, self); err != nil {
		return fmt.Errorf("write ssh config fragment: %v", err)
	}

	managedPath := filepath.Join(dir, managedConfigName)
	wired, err := ensureInclude(managedPath)
	if err != nil {
		return err
	}

	printConfirmation(cmd.OutOrStdout(), bundle, suffix, managedPath, wired)
	return nil
}

// runProxy reloads the stored state, strips the cluster suffix from the ssh
// hostname to recover the VM name, and relays stdin/stdout through the
// connector.
func runProxy(cmd *cobra.Command, host, portArg string) error {
	port, err := strconv.Atoi(portArg)
	if err != nil {
		return fmt.Errorf("invalid port %q: %v", portArg, err)
	}
	dir, err := stateDir()
	if err != nil {
		return err
	}
	st, err := loadState(dir)
	if err != nil {
		return err
	}
	vmName := stripSuffix(host, st.ClusterSuffix)
	cfg := sshConfigFromState(st, dir)
	// Mint (or refresh) the guest certificate before relaying so the
	// CertificateFile the managed ssh_config points at is present when the ssh
	// client reads it during user authentication. The grant token authorizes
	// the mint and the Control Plane pins the login, so no login is passed.
	if _, _, err := ensureCert(cmd.Context(), cfg, vmName, ""); err != nil {
		return err
	}
	return proxyConnect(cmd.Context(), cfg, vmName, port, cmd.InOrStdin(), cmd.OutOrStdout())
}

// readBundle returns the bundle text from arg: "-" reads stdin, an existing
// file path reads that file, otherwise arg is treated as the bundle string
// itself (the operator's paste-able blob is a single line).
func readBundle(arg string, stdin io.Reader) (string, error) {
	if arg == "-" {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return "", fmt.Errorf("read bundle from stdin: %v", err)
		}
		return string(data), nil
	}
	if info, err := os.Stat(arg); err == nil && !info.IsDir() {
		data, err := os.ReadFile(arg) //nolint:gosec // G304: arg is the operator-supplied bundle path the external person chose to import.
		if err != nil {
			return "", fmt.Errorf("read bundle file %q: %v", arg, err)
		}
		return string(data), nil
	}
	return arg, nil
}

// stripSuffix removes a trailing ".<suffix>" from host, recovering the bare VM
// name ssh's wildcard match passed through %h. A host without the suffix (or an
// empty suffix) is returned unchanged.
func stripSuffix(host, suffix string) string {
	if suffix == "" {
		return host
	}
	return strings.TrimSuffix(host, "."+suffix)
}

// sshConfigFromState builds the sshconn.Config the connector operations
// consume from the persisted state, pinning KnownDir to the connector's own
// state dir so the cached identity and managed fragment stay co-located.
func sshConfigFromState(st connectorState, dir string) sshconn.Config {
	return sshconn.Config{
		ServerURL:             st.ServerURL,
		CAFingerprint:         st.CAFingerprint,
		CACertPEM:             st.CACertPEM,
		InsecureSkipTLSVerify: st.InsecureSkipTLSVerify,
		BearerToken:           st.Token,
		KnownDir:              dir,
	}
}

// stateDir returns ~/.otherix/ssh, the connector's per-user state directory.
func stateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %v", err)
	}
	return filepath.Join(append([]string{home}, stateDirParts...)...), nil
}

// writeState persists st to {dir}/state.json with 0600 perms. It writes to a
// temp file in the same dir and renames into place so a crash mid-write never
// leaves a half-written state file. The grant token inside is a bearer secret;
// the 0600 mode keeps it readable only by the owner.
func writeState(dir string, st connectorState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %v", err)
	}
	final := filepath.Join(dir, stateFileName)
	tmp, err := os.CreateTemp(dir, stateFileName+".*")
	if err != nil {
		return fmt.Errorf("create temp state file: %v", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp state file: %v", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp state file: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp state file: %v", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return fmt.Errorf("install state file: %v", err)
	}
	return nil
}

// loadState reads and decodes {dir}/state.json.
func loadState(dir string) (connectorState, error) {
	path := filepath.Join(dir, stateFileName)
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the connector's own state file under the operator-controlled home dir.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return connectorState{}, errors.New("no stored connection: run 'otherix-ssh add <bundle>' first")
		}
		return connectorState{}, fmt.Errorf("read state file: %v", err)
	}
	var st connectorState
	if err := json.Unmarshal(data, &st); err != nil {
		return connectorState{}, fmt.Errorf("decode state file: %v", err)
	}
	return st, nil
}

// ensureInclude makes ~/.ssh/config pull in the managed fragment via an
// `Include {managedPath}` line, adding it only when absent so the operator's
// own config is never clobbered or duplicated. It returns whether the line was
// newly added. It creates ~/.ssh (0700) and ~/.ssh/config (0600) when missing.
func ensureInclude(managedPath string) (bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, fmt.Errorf("resolve home dir: %v", err)
	}
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return false, fmt.Errorf("create ~/.ssh: %v", err)
	}
	cfgPath := filepath.Join(sshDir, "config")
	includeLine := "Include " + managedPath

	existing, err := os.ReadFile(cfgPath) //nolint:gosec // G304: the user's own ~/.ssh/config the connector wires its Include into.
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("read ~/.ssh/config: %v", err)
	}
	if includeAlreadyPresent(string(existing), includeLine) {
		return false, nil
	}

	// Prepend the Include so the managed wildcard Host block applies globally:
	// an Include placed after a `Host X` stanza would inherit that stanza's
	// match condition. A trailing blank line keeps it visually separate from
	// the operator's own content, which is preserved verbatim below it.
	var b strings.Builder
	b.WriteString(includeMarker)
	b.WriteByte('\n')
	b.WriteString(includeLine)
	b.WriteString("\n\n")
	b.Write(existing)
	if err := os.WriteFile(cfgPath, []byte(b.String()), 0o600); err != nil {
		return false, fmt.Errorf("write ~/.ssh/config: %v", err)
	}
	return true, nil
}

// includeAlreadyPresent reports whether any line of cfg is exactly the
// Include directive (ignoring surrounding whitespace).
func includeAlreadyPresent(cfg, includeLine string) bool {
	for _, line := range strings.Split(cfg, "\n") {
		if strings.TrimSpace(line) == includeLine {
			return true
		}
	}
	return false
}

// printConfirmation prints the post-import summary: the granted VMs and the
// `ssh <vm>.<suffix>` form. When the Include was not auto-added (already
// present, or a prior write the user must apply), it prints the exact line so
// the user can wire it manually. The grant token is never printed.
func printConfirmation(w io.Writer, bundle sshgrant.Bundle, suffix, managedPath string, wired bool) {
	_, _ = fmt.Fprintf(w, "Imported grant for %s\n", bundle.ServerURL)
	if len(bundle.VMs) > 0 {
		_, _ = fmt.Fprintln(w, "You can now connect to:")
		for _, vm := range bundle.VMs {
			login := vm.Login
			if login == "" {
				login = "root"
			}
			_, _ = fmt.Fprintf(w, "  ssh %s@%s.%s\n", login, vm.VM, suffix)
		}
	}
	if !wired {
		_, _ = fmt.Fprintf(w, "\nIf ssh cannot find a VM, ensure ~/.ssh/config contains:\n  Include %s\n", managedPath)
	}
}
