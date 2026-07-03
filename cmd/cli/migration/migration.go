// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package migration hosts the `otherix migration` cobra subcommand group
// (get / list / cancel) against the Control Plane's /v1/migrations surface. A
// migration is created through `otherix vm migrate` (it is a VM sub-resource
// action); this group polls a migration's phase / progress, lists migrations
// filtered by VM or node, and cancels a migration (best-effort, pre-cutover).
// Heavy lifting lives in cmd/cli/internal/cpclient; this package owns flag
// plumbing and output formatting.
package migration

import (
	"github.com/spf13/cobra"
)

// NewCommand returns the `otherix migration` subcommand group, ready to be
// registered onto the root cobra tree by main.
func NewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migration",
		Short: "Inspect VM migrations",
		Long: `migration groups the operator subcommands against the Control
Plane's /v1/migrations surface — get a migration by id, list migrations
filtered by VM or node, and cancel a migration (best-effort, pre-cutover).
Migrations are *created* through 'otherix vm migrate <vm>' (a VM sub-resource
action), never here. Authentication and endpoint resolution flow through the
root-level --endpoint / --token / --cluster / --config flags.`,
	}

	cmd.AddCommand(newGetCommand())
	cmd.AddCommand(newListCommand())
	cmd.AddCommand(newCancelCommand())

	return cmd
}
