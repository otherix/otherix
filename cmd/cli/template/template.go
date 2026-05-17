// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package template hosts the `otherix template` cobra subcommand group
// и its children — create, get, list, delete. `create` is composite:
// it registers а template и, when --pool is provided, triggers а
// storage_image.import task on the pool's agent. The PATCH /
// set-visibility surface is still admin-friendly via direct API calls
// pending dedicated CLI verbs.
package template

import "github.com/spf13/cobra"

// NewCommand returns the `otherix template` subcommand group, ready
// к be registered onto the root cobra tree by main.
func NewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "template",
		Short: "Manage VM templates (CP /v1/templates surface)",
		Long: `template groups the operator subcommands против the Control Plane's
/v1/templates surface — create, get, list, delete. The PATCH и
set-visibility surfaces stay admin-friendly via direct API calls until
dedicated CLI verbs land.`,
	}
	cmd.AddCommand(newCreateCommand())
	cmd.AddCommand(newGetCommand())
	cmd.AddCommand(newListCommand())
	cmd.AddCommand(newDeleteCommand())
	return cmd
}
