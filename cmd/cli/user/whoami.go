// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package user

import "github.com/spf13/cobra"

// newWhoamiCommand returns the `otherix user whoami` cobra command. It
// fetches GET /v1/users/me and prints a one-line identity summary of the
// authenticated caller. Available to every authenticated role; `-o json`
// / `-o yaml` emit the full user object.
func newWhoamiCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "whoami",
		Short: "Show who you are authenticated as (one-line summary).",
		Long: `Fetches GET /v1/users/me and prints a one-line summary of the
caller (username and role). Available to every authenticated role.
Use -o json or -o yaml for the full user object.`,
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
	// whoami is "who am I" - a terse identity line by default. The full
	// record is available via `user get <username>` or -o json/yaml here.
	if format == "text" {
		printf(cmd, "%s (role: %s)\n", me.Username, me.Role)
		return nil
	}
	return renderUser(cmd, me, format)
}
