// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package user

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// newSetRoleCommand returns the `otherix user set-role` cobra command.
// It resolves <username> to its UUID and PATCHes the role.
func newSetRoleCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set-role <username> <role>",
		Short: "Change a user's role (admin-only).",
		Long: `Submits PATCH /v1/users/{id} with a new role. Required permission:
user:manage (admin role). <role> is one of admin|operator|developer|
viewer. The username is resolved to its UUID client-side.

  otherix user set-role web-admin operator`,
		Args: cobra.ExactArgs(2),
		RunE: runSetRole,
	}
	cmd.Flags().StringP(flagOutput, "o", "text", "output format: text|json|yaml")
	return cmd
}

func runSetRole(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	username, role := args[0], args[1]
	if username == "" {
		return errors.New("username is required")
	}
	if role == "" {
		return errors.New("role is required")
	}
	format, err := outputFormat(cmd, "text", "yaml")
	if err != nil {
		return err
	}

	u, err := resolveUserID(cmd.Context(), c, username)
	if err != nil {
		return err
	}

	updated, err := c.UpdateUser(cmd.Context(), u.ID, cpclient.UpdateUserRequest{Role: &role})
	if err != nil {
		return classifyError(err)
	}

	if format == "text" {
		printf(cmd, "user %s role set to %s\n", updated.Username, updated.Role)
		return nil
	}
	return renderUser(cmd, updated, format)
}
