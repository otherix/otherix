// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package sshgrant

import (
	"errors"
	"strings"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// newGetCommand returns `otherix ssh-grant get <id|name>`.
func newGetCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <id|name>",
		Short: "Show one SSH grant.",
		Long: `Shows a single SSH grant by id or name. The stored token is never
surfaced (it is shown only once, in the create bundle).`,
		Args: cobra.ExactArgs(1),
		RunE: runGet,
	}
	cmd.Flags().StringP(flagOutput, "o", "text", "output format: text|json|yaml")
	return cmd
}

func runGet(cmd *cobra.Command, args []string) error {
	identifier := strings.TrimSpace(args[0])
	if identifier == "" {
		return errors.New("id or name is required")
	}
	format, err := outputFormat(cmd, "text", "yaml")
	if err != nil {
		return err
	}
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	grant, raw, err := c.GetSSHGrant(cmd.Context(), identifier)
	if err != nil {
		if errors.Is(err, cpclient.ErrSSHGrantNotFound) {
			return errors.New("ssh grant not found: " + identifier)
		}
		return classifyError(err)
	}
	return renderGrant(cmd, grant, raw, format)
}
