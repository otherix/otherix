// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package user

import (
	"errors"

	"github.com/spf13/cobra"
)

// newGetCommand returns the `otherix user get` cobra command. The
// positional <username> is resolved to its row via the username guard
// lookup.
func newGetCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <username>",
		Short: "Show a user by username.",
		Long: `Fetches a single user by username (the immutable identity). The
username is resolved through the server-side guard lookup; a missing
user surfaces as a clean "user not found" error.`,
		Args: cobra.ExactArgs(1),
		RunE: runGet,
	}
	cmd.Flags().StringP(flagOutput, "o", "text", "output format: text|json|yaml")
	return cmd
}

func runGet(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	username := args[0]
	if username == "" {
		return errors.New("username is required")
	}
	format, err := outputFormat(cmd, "text", "yaml")
	if err != nil {
		return err
	}

	u, err := resolveUserID(cmd.Context(), c, username)
	if err != nil {
		return err
	}
	return renderUser(cmd, u, format)
}
