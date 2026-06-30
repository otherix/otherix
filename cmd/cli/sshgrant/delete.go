// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package sshgrant

import (
	"errors"
	"strings"

	"github.com/spf13/cobra"
)

// newDeleteCommand returns `otherix ssh-grant delete <id|name>`.
func newDeleteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <id|name>",
		Short: "Delete an SSH grant.",
		Long: `Deletes an SSH grant by id or name. The grant's token stops working
immediately and the grant's name is freed for reuse. Unlike 'revoke', which
disables the grant but keeps it for audit, 'delete' removes the grant entirely.`,
		Args: cobra.ExactArgs(1),
		RunE: runDelete,
	}
	return cmd
}

func runDelete(cmd *cobra.Command, args []string) error {
	identifier := strings.TrimSpace(args[0])
	if identifier == "" {
		return errors.New("id or name is required")
	}
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	if err := c.DeleteSSHGrant(cmd.Context(), identifier); err != nil {
		return mapGrantError(err, identifier)
	}
	printf(cmd, "ssh grant %q deleted\n", identifier)
	return nil
}
