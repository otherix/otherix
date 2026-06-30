// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package sshgrant

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	"github.com/otherix/otherix/cmd/cli/internal/cliconfig"
	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// newCreateCommand returns `otherix ssh-grant create <name>`. The positional
// <name> labels the grant. --vm is the comma-separated VM scope (each entry may
// inline its own login as name=login); --login is the default guest login;
// --ttl bounds the grant's lifetime; --user is an optional recipient label. The
// shareable bundle (carrying the one-time grant token) is printed on success.
func newCreateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Mint an SSH grant and print its shareable bundle.",
		Long: `Mints an SSH grant for an external user and prints a bundle to share
with them. The bundle carries the control-plane URL, the TLS trust the external
needs to reach the same control plane you do, the one-time grant token, and the
granted vm:login set. The external imports it with 'otherix-ssh add'.

--vm is a comma list of VMs the grant authorizes; give a per-VM login inline as
'name=login' or a shared default with --login (default 'root'). --ttl bounds the
lifetime (e.g. 168h, 720h); omit it for a grant that never expires. --user is an
optional label naming the recipient.

The grant token is shown exactly once, in the bundle; it is never a flag or
argument.

  otherix ssh-grant create alice-web --vm web01,web02 --login deploy --ttl 168h
  otherix ssh-grant create dbadmin --vm db01=postgres --user "Alice Smith"
  otherix ssh-grant create ci --vm web01 --ttl 24h -o json   # bundle as JSON`,
		Args: cobra.ExactArgs(1),
		RunE: runCreate,
	}
	cmd.Flags().String(flagVM, "", "comma-separated VMs to grant (each may inline a login as name=login)")
	cmd.Flags().String(flagLogin, "", "default guest login for VMs without an inline login (default "+defaultLogin+")")
	cmd.Flags().String(flagTTL, "", "grant lifetime as a duration (168h, 720h); default: never expires")
	cmd.Flags().String(flagUser, "", "recipient label naming the external user")
	cmd.Flags().StringP(flagOutput, "o", "text", "output format: text|json|yaml")
	return cmd
}

func runCreate(cmd *cobra.Command, args []string) error {
	name := strings.TrimSpace(args[0])
	if name == "" {
		return errors.New("name is required")
	}
	format, err := outputFormat(cmd, "text", "yaml")
	if err != nil {
		return err
	}
	vmList, _ := cmd.Flags().GetString(flagVM)
	login, _ := cmd.Flags().GetString(flagLogin)
	ttl, _ := cmd.Flags().GetString(flagTTL)
	recipient, _ := cmd.Flags().GetString(flagUser)

	vms, err := parseVMScope(vmList, login)
	if err != nil {
		return err
	}

	// Resolve the operator's own trust before the mutating call so a
	// mis-configured cluster fails before a grant is created.
	auth, err := cliauth.ResolveAuth(cmd)
	if err != nil {
		return err
	}

	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}

	grant, err := c.CreateSSHGrant(cmd.Context(), cpclient.CreateSSHGrantRequest{
		Name:           name,
		RecipientLabel: strings.TrimSpace(recipient),
		VMs:            vms,
		TTL:            strings.TrimSpace(ttl),
	})
	if err != nil {
		if errors.Is(err, cpclient.ErrSSHGrantExists) {
			return fmt.Errorf("ssh grant %q already exists", name)
		}
		return classifyError(err)
	}

	bundle := buildBundle(auth, grant)
	return renderCreated(cmd, grant, bundle, format)
}

// buildBundle assembles the shareable Bundle from the operator's resolved CLI
// trust and the created grant. The granted vm:login set is taken from the
// server's response (authoritative, with logins normalised), and the
// TLS-trust discriminator is derived from the operator's OWN trust so the
// external reaches the same control plane.
func buildBundle(auth cliconfig.ResolvedAuth, grant cpclient.SSHGrant) Bundle {
	trust, caPEM := bundleTrust(auth)
	vms := make([]BundleVM, 0, len(grant.VMs))
	for _, vm := range grant.VMs {
		vms = append(vms, BundleVM{VM: vm.VMName, Login: vm.Login})
	}
	return Bundle{
		Version:   BundleVersion,
		ServerURL: auth.Endpoint,
		Trust:     trust,
		CACertPEM: caPEM,
		Token:     grant.Token,
		VMs:       vms,
	}
}

// bundleTrust derives the bundle's trust discriminator from the operator's
// resolved CLI trust. InsecureSkipTLSVerify wins (explicit opt-out), then a
// configured CA bundle (the cluster CA), else system roots. The CA bundle is
// carried verbatim as the already-base64 ResolvedAuth.CACertData so the
// external trusts the same chain. The leaf-pin form is never produced here -
// the operator config carries a CA bundle, not a fingerprint - but the
// connector still accepts it.
func bundleTrust(auth cliconfig.ResolvedAuth) (trust, caCertPEM string) {
	switch {
	case auth.InsecureSkipTLSVerify:
		return TrustInsecure, ""
	case auth.CACertData != "":
		return TrustCABundle, auth.CACertData
	default:
		return TrustWebPKI, ""
	}
}

// renderCreated prints the create result. text prints a human summary, the
// single-line paste-able bundle blob, and a hand-off hint. json/yaml emit the
// bundle structure itself (token included) for capture by automation.
func renderCreated(cmd *cobra.Command, grant cpclient.SSHGrant, bundle Bundle, format string) error {
	switch format {
	case "json":
		out, err := json.MarshalIndent(bundle, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal bundle json: %v", err)
		}
		printf(cmd, "%s\n", out)
		return nil
	case "yaml":
		out, err := yaml.Marshal(bundle)
		if err != nil {
			return fmt.Errorf("marshal bundle yaml: %v", err)
		}
		printf(cmd, "%s", out)
		return nil
	}

	blob, err := EncodeBundle(bundle)
	if err != nil {
		return err
	}
	printf(cmd, "ssh grant %q created. Send the bundle below to %s; it carries\n",
		grant.Name, recipientPhrase(grant.RecipientLabel))
	printf(cmd, "the access token and is shown only once.\n\n")
	printf(cmd, "  server:  %s\n", bundle.ServerURL)
	printf(cmd, "  trust:   %s\n", bundle.Trust)
	printf(cmd, "  expires: %s\n", derefOr(grant.ExpiresAt, "never"))
	printf(cmd, "  vms:\n")
	for _, vm := range bundle.VMs {
		printf(cmd, "    - %s (login %s)\n", vm.VM, vm.Login)
	}
	printf(cmd, "\nbundle (paste-able - the external runs `otherix-ssh add <bundle>`):\n\n")
	printf(cmd, "  %s\n", blob)
	return nil
}

// recipientPhrase renders a hand-off phrase for the create summary.
func recipientPhrase(label string) string {
	if strings.TrimSpace(label) == "" {
		return "the external user"
	}
	return label
}
