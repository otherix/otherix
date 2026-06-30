// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package sshgrant

import (
	"errors"
	"strings"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// newAddVMCommand returns `otherix ssh-grant add-vm <id|name> <vm> [--login L]`.
func newAddVMCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add-vm <id|name> <vm>",
		Short: "Add a VM to an SSH grant's scope.",
		Long: `Adds a VM to the grant's scope (or replaces its login when already
present). --login sets the guest login (default ` + defaultLogin + `).`,
		Args: cobra.ExactArgs(2),
		RunE: runAddVM,
	}
	cmd.Flags().String(flagLogin, "", "guest login for the VM (default "+defaultLogin+")")
	cmd.Flags().StringP(flagOutput, "o", "text", "output format: text|json|yaml")
	return cmd
}

func runAddVM(cmd *cobra.Command, args []string) error {
	identifier := strings.TrimSpace(args[0])
	vmName := strings.TrimSpace(args[1])
	if identifier == "" || vmName == "" {
		return errors.New("grant id/name and vm are required")
	}
	login, _ := cmd.Flags().GetString(flagLogin)
	format, err := outputFormat(cmd, "text", "yaml")
	if err != nil {
		return err
	}
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	grant, err := c.AddSSHGrantVM(cmd.Context(), identifier, cpclient.SSHGrantVM{
		VMName: vmName,
		Login:  effectiveLogin(login),
	})
	if err != nil {
		return mapGrantError(err, identifier)
	}
	if format == "text" {
		printf(cmd, "added %s (login %s) to ssh grant %q\n", vmName, effectiveLogin(login), grant.Name)
		return nil
	}
	return renderGrant(cmd, grant, nil, format)
}

// newRemoveVMCommand returns `otherix ssh-grant remove-vm <id|name> <vm>`.
func newRemoveVMCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove-vm <id|name> <vm>",
		Short: "Remove a VM from an SSH grant's scope.",
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
	grant, err := c.RemoveSSHGrantVM(cmd.Context(), identifier, vmName)
	if err != nil {
		return mapGrantError(err, identifier)
	}
	if format == "text" {
		printf(cmd, "removed %s from ssh grant %q\n", vmName, grant.Name)
		return nil
	}
	return renderGrant(cmd, grant, nil, format)
}

// mapGrantError collapses the name-resolution miss to a clean message and
// otherwise classifies the cpclient error.
func mapGrantError(err error, identifier string) error {
	if errors.Is(err, cpclient.ErrSSHGrantNotFound) {
		return errors.New("ssh grant not found: " + identifier)
	}
	return classifyError(err)
}
