// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package user

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// newCreateCommand returns the `otherix user create` cobra command. The
// positional <username> is the new user's immutable identity. --role is
// required. The password is never a flag or argument: it is read from an
// interactive no-echo prompt, or from stdin when --password-stdin is set.
func newCreateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <username>",
		Short: "Create a user (admin-only).",
		Long: `Submits POST /v1/users. Required permission: user:manage (admin
role). The positional <username> is the new user's immutable identity
and only login credential (lowercase letters, digits, interior hyphens;
3-32 chars). --role is required. --email and --display-name are
optional.

The password is never passed as a flag or argument (that would leak it
into shell history and the process table). It is read from an
interactive no-echo prompt, or - for automation - from stdin when
--password-stdin is given:

  otherix user create web-admin --role developer
  otherix user create ci-bot --role viewer --password-stdin <<<"$PW"`,
		Args: cobra.ExactArgs(1),
		RunE: runCreate,
	}
	cmd.Flags().String(flagEmail, "", "optional email address")
	cmd.Flags().String(flagDisplayName, "", "optional human-readable display name")
	cmd.Flags().String(flagRole, "", "role: admin|operator|developer|viewer (required)")
	cmd.Flags().Bool(flagPasswordStdin, false, "read the password from stdin (one line) instead of prompting")
	cmd.Flags().StringP(flagOutput, "o", "text", "output format: text|json")
	_ = cmd.MarkFlagRequired(flagRole)
	return cmd
}

func runCreate(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	username := args[0]
	if username == "" {
		return errors.New("username is required")
	}
	role, err := cmd.Flags().GetString(flagRole)
	if err != nil {
		return err
	}
	if role == "" {
		return errors.New("--role is required")
	}
	email, _ := cmd.Flags().GetString(flagEmail)
	displayName, _ := cmd.Flags().GetString(flagDisplayName)
	format, err := outputFormat(cmd, "text")
	if err != nil {
		return err
	}

	password, err := readPassword(cmd, fmt.Sprintf("Password for %s: ", username))
	if err != nil {
		return err
	}

	created, err := c.CreateUser(cmd.Context(), cpclient.CreateUserRequest{
		Username:    username,
		Email:       email,
		DisplayName: displayName,
		Role:        role,
		Password:    password,
	})
	if err != nil {
		return classifyError(err)
	}

	if format == "text" {
		printf(cmd, "user %s created\n", created.Username)
	}
	return renderUser(cmd, created, format)
}
