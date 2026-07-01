// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package ingressgrant hosts the `otherix ingress-grant` cobra subcommand group over
// the Control Plane's /v1/ingress-grants surface. An ingress grant is a per-person
// access credential an external user (one without a CLI account) presents to
// reach a fixed set of VMs over the control-plane SSH relay.
//
// `create` mints a grant and prints a shareable bundle (server URL + TLS-trust
// discriminator + the one-time grant token + the granted {vm, login} set) the
// operator hands to the external person; `list` / `get` inspect grants; `add-vm`
// / `remove-vm` adjust a grant's mutable VM scope; `revoke` disables it.
//
// The whole surface is gated by vm:ingress-grant (admin/operator any, developer
// own, viewer none). The plaintext grant token is surfaced exactly once, in
// the create bundle, and is never accepted as a flag or argument.
package ingressgrant

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	"github.com/otherix/otherix/cmd/cli/internal/clierr"
	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// Flag names shared across the ingress-grant subcommands.
const (
	flagVM        = "vm"
	flagLogin     = "login"
	flagTTL       = "ttl"
	flagUser      = "user"
	flagOutput    = "output"
	flagLimit     = "limit"
	flagCursor    = "cursor"
	flagShowToken = "show-token"
)

// defaultLogin is the guest login a VM scope entry takes when --login is
// omitted. It mirrors the connector's own default principal.
const defaultLogin = "root"

// defaultListLimit is the page size `list` requests when --limit is unset.
const defaultListLimit = 20

// NewCommand returns the `otherix ingress-grant` subcommand group, ready to be
// registered onto the root cobra tree by main.
func NewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ingress-grant",
		Short: "Manage SSH access grants for external users.",
		Long: `ingress-grant manages per-person SSH access grants. A grant lets an
external user - one without a CLI account - reach a fixed set of VMs over the
control-plane SSH relay using a single shareable token.

'create' mints a grant and prints a bundle (server URL, TLS trust, the
one-time grant token, and the granted vm:login set) to hand to the external
person; they import it with 'otherix-ssh add'. Manage a grant's VM scope with
'add-vm' / 'remove-vm' and disable it with 'revoke'.

The whole surface is gated by vm:ingress-grant. The plaintext grant token is shown
exactly once, in the create bundle, and is never a flag or argument.`,
	}
	cmd.AddCommand(newCreateCommand())
	cmd.AddCommand(newListCommand())
	cmd.AddCommand(newGetCommand())
	cmd.AddCommand(newAddVMCommand())
	cmd.AddCommand(newRemoveVMCommand())
	cmd.AddCommand(newRevokeCommand())
	cmd.AddCommand(newDeleteCommand())
	return cmd
}

// clientFromFlags builds an authenticated CP client from the inherited
// persistent flags + env + config file.
func clientFromFlags(cmd *cobra.Command) (*cpclient.Client, error) {
	return cliauth.BuildClient(cmd)
}

// printf writes to the command's configured stdout (test-redirectable).
func printf(cmd *cobra.Command, format string, args ...any) {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), format, args...)
}

// classifyError maps a cpclient error to a stable-prefixed operator message
// shared with the other CLI surfaces.
func classifyError(err error) error {
	return clierr.Classify(err)
}

// outputFormat reads --output (default defaultFormat). text/json are always
// accepted; extra opts into more (yaml). Unknown -> usage error.
func outputFormat(cmd *cobra.Command, defaultFormat string, extra ...string) (string, error) {
	raw, _ := cmd.Flags().GetString(flagOutput)
	if raw == "" {
		raw = defaultFormat
	}
	allowed := append([]string{"text", "json"}, extra...)
	if slices.Contains(allowed, raw) {
		return raw, nil
	}
	return "", fmt.Errorf("--%s: unknown format %q (%s)", flagOutput, raw, strings.Join(allowed, ", "))
}

// parseVMScope turns a --vm comma list plus the --login default into the VM
// scope entries the create request carries. Each entry may inline its own
// login via "name=login"; otherwise the default login applies. Blank entries
// are skipped; a duplicate VM name is an error (the server would reject it too,
// but a clean client-side message is friendlier).
func parseVMScope(vmList, login string) ([]cpclient.IngressGrantVM, error) {
	login = effectiveLogin(login)
	var out []cpclient.IngressGrantVM
	seen := make(map[string]struct{})
	for _, raw := range strings.Split(vmList, ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		entryLogin := login
		if k, v, ok := strings.Cut(name, "="); ok {
			name = strings.TrimSpace(k)
			entryLogin = effectiveLogin(strings.TrimSpace(v))
			if name == "" {
				return nil, fmt.Errorf("invalid --%s entry %q: empty vm name", flagVM, raw)
			}
		}
		if _, dup := seen[name]; dup {
			return nil, fmt.Errorf("duplicate vm in --%s: %q", flagVM, name)
		}
		seen[name] = struct{}{}
		out = append(out, cpclient.IngressGrantVM{VMName: name, Login: entryLogin})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("at least one VM is required: pass --%s web01,web02", flagVM)
	}
	return out, nil
}

// effectiveLogin returns login, defaulting blank/whitespace to defaultLogin.
func effectiveLogin(login string) string {
	if strings.TrimSpace(login) == "" {
		return defaultLogin
	}
	return strings.TrimSpace(login)
}

// renderGrant writes a single grant view in json/yaml; the text branch prints
// a compact identity block. raw is the server's verbatim by-id JSON when
// available (preserves absent-vs-null on the json path); pass nil to marshal
// the typed value instead.
func renderGrant(cmd *cobra.Command, g cpclient.IngressGrant, raw json.RawMessage, format string) error {
	switch format {
	case "json":
		if len(raw) > 0 {
			var buf bytes.Buffer
			if err := json.Indent(&buf, raw, "", "  "); err == nil {
				printf(cmd, "%s\n", buf.String())
				return nil
			}
		}
		out, err := json.MarshalIndent(g, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", out)
	case "yaml":
		out, err := yaml.Marshal(g)
		if err != nil {
			return fmt.Errorf("marshal yaml: %v", err)
		}
		printf(cmd, "%s", out)
	default:
		printGrantText(cmd, g)
	}
	return nil
}

// printGrantText renders the human-readable single-grant block.
func printGrantText(cmd *cobra.Command, g cpclient.IngressGrant) {
	printf(cmd, "id:         %s\n", g.ID)
	printf(cmd, "name:       %s\n", g.Name)
	if g.RecipientLabel != "" {
		printf(cmd, "recipient:  %s\n", g.RecipientLabel)
	}
	printf(cmd, "status:     %s\n", grantStatus(g))
	printf(cmd, "expires_at: %s\n", derefOr(g.ExpiresAt, "never"))
	printf(cmd, "vms:\n")
	for _, vm := range g.VMs {
		printf(cmd, "  - %s (login %s)\n", vm.VMName, vm.Login)
	}
}

// grantStatus derives a display status: revoked beats active.
func grantStatus(g cpclient.IngressGrant) string {
	if g.Revoked {
		return "revoked"
	}
	return "active"
}

// derefOr returns *p or fallback when p is nil/empty.
func derefOr(p *string, fallback string) string {
	if p == nil || *p == "" {
		return fallback
	}
	return *p
}
