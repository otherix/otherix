// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package user

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// newSetPasswordCommand returns the `otherix user set-password` cobra
// command. The password is never a flag or argument: it is read from an
// interactive no-echo prompt, or from stdin when --password-stdin is set.
func newSetPasswordCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set-password <username>",
		Short: "Set a user's password (admin-only).",
		Long: `Submits PATCH /v1/users/{id} with a new password. Required
permission: user:manage (admin role). The username is resolved to its
UUID client-side.

The password is never passed as a flag or argument (that would leak it
into shell history and the process table). It is read from an
interactive no-echo prompt, or - for automation - from stdin when
--password-stdin is given:

  otherix user set-password web-admin
  otherix user set-password ci-bot --password-stdin <<<"$NEWPW"`,
		Args: cobra.ExactArgs(1),
		RunE: runSetPassword,
	}
	cmd.Flags().Bool(flagPasswordStdin, false, "read the password from stdin (one line) instead of prompting")
	cmd.Flags().String(flagOutput, "text", "output format: text|json")
	return cmd
}

func runSetPassword(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	username := args[0]
	if username == "" {
		return errors.New("username is required")
	}
	format, err := outputFormat(cmd, "text")
	if err != nil {
		return err
	}

	u, err := resolveUserID(cmd.Context(), c, username)
	if err != nil {
		return err
	}

	password, err := readPassword(cmd, fmt.Sprintf("New password for %s: ", username))
	if err != nil {
		return err
	}

	if _, err := c.UpdateUser(cmd.Context(), u.ID, cpclient.UpdateUserRequest{Password: &password}); err != nil {
		return classifyError(err)
	}

	switch format {
	case "json":
		printf(cmd, "{\"updated\":true,\"username\":%q}\n", username)
	default:
		printf(cmd, "password updated for user %s\n", username)
	}
	return nil
}
