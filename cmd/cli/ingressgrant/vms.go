// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package ingressgrant

import (
	"errors"
	"strings"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// newAddVMCommand returns
// `otherix ingress-grant add-vm <id|name> <vm:port[,port...]> [--login L]`.
func newAddVMCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add-vm <id|name> <vm:port[,port...]>",
		Short: "Add a VM to an ingress grant's scope.",
		Long: `Adds a VM to the grant's scope (or replaces its ports and login when the VM
is already present). The VM argument is host:port[,port...] naming the guest
TCP ports the grant authorizes (port 22 is SSH). --login sets the guest login
(default ` + defaultLogin + `).

  otherix ingress-grant add-vm alice-web db01:5432
  otherix ingress-grant add-vm alice-web web:22,8080 --login deploy`,
		Args: cobra.ExactArgs(2),
		RunE: runAddVM,
	}
	cmd.Flags().String(flagLogin, "", "guest login for the VM (default "+defaultLogin+")")
	cmd.Flags().StringP(flagOutput, "o", "text", "output format: text|json|yaml")
	return cmd
}

func runAddVM(cmd *cobra.Command, args []string) error {
	identifier := strings.TrimSpace(args[0])
	vmEntry := strings.TrimSpace(args[1])
	if identifier == "" || vmEntry == "" {
		return errors.New("grant id/name and vm are required")
	}
	login, _ := cmd.Flags().GetString(flagLogin)
	format, err := outputFormat(cmd, "text", "yaml")
	if err != nil {
		return err
	}
	// Reuse the create command's per-entry parser: a single host:port[,port...]
	// entry yields exactly one IngressGrantVM carrying the required non-empty
	// port set plus the resolved login.
	vms, err := parseVMScope([]string{vmEntry}, login)
	if err != nil {
		return err
	}
	vm := vms[0]
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	grant, err := c.AddIngressGrantVM(cmd.Context(), identifier, vm)
	if err != nil {
		return mapGrantError(err, identifier)
	}
	if format == "text" {
		printf(cmd, "added %s ports %s (login %s) to ingress grant %q\n",
			vm.VMName, portsDisplay(vm.Ports), vm.Login, grant.Name)
		return nil
	}
	return renderGrant(cmd, grant, nil, format)
}

// newRemoveVMCommand returns `otherix ingress-grant remove-vm <id|name> <vm>`.
func newRemoveVMCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove-vm <id|name> <vm>",
		Short: "Remove a VM from an ingress grant's scope.",
		Long: `Removes a VM from the grant's scope. Removing a VM that is not in the
grant is a no-op.`,
		Args: cobra.ExactArgs(2),
		RunE: runRemoveVM,
	}
	cmd.Flags().StringP(flagOutput, "o", "text", "output format: text|json|yaml")
	return cmd
}

func runRemoveVM(cmd *cobra.Command, args []string) error {
	identifier := strings.TrimSpace(args[0])
	vmName := strings.TrimSpace(args[1])
	if identifier == "" || vmName == "" {
		return errors.New("grant id/name and vm are required")
	}
	format, err := outputFormat(cmd, "text", "yaml")
	if err != nil {
		return err
	}
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	grant, err := c.RemoveIngressGrantVM(cmd.Context(), identifier, vmName)
	if err != nil {
		return mapGrantError(err, identifier)
	}
	if format == "text" {
		printf(cmd, "removed %s from ingress grant %q\n", vmName, grant.Name)
		return nil
	}
	return renderGrant(cmd, grant, nil, format)
}

// mapGrantError collapses the name-resolution miss to a clean message and
// otherwise classifies the cpclient error.
func mapGrantError(err error, identifier string) error {
	if errors.Is(err, cpclient.ErrIngressGrantNotFound) {
		return errors.New("ingress grant not found: " + identifier)
	}
	return classifyError(err)
}
