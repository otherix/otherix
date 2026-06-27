// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package user

import "github.com/spf13/cobra"

// newWhoamiCommand returns the `otherix user whoami` cobra command. It
// fetches GET /v1/users/me and renders the authenticated caller's own
// user view. Available to every authenticated role.
func newWhoamiCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "whoami",
		Short: "Show the authenticated caller's own user.",
		Long: `Fetches GET /v1/users/me and renders the caller's own user view.
Available to every authenticated role.`,
		Args: cobra.NoArgs,
		RunE: runWhoami,
	}
	cmd.Flags().StringP(flagOutput, "o", "text", "output format: text|json|yaml")
	return cmd
}

func runWhoami(cmd *cobra.Command, _ []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	format, err := outputFormat(cmd, "text", "yaml")
	if err != nil {
		return err
	}

	me, err := c.GetMe(cmd.Context())
	if err != nil {
		return classifyError(err)
	}
	return renderUser(cmd, me, format)
}
