// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package migration hosts the `otherix migration` cobra subcommand group
// (get / list) against the Control Plane's /v1/migrations surface. A
// migration is created through `otherix vm migrate` (it is a VM sub-resource
// action); this group is the read side — poll a migration's phase / progress
// and list migrations filtered by VM or node. Heavy lifting lives in
// cmd/cli/internal/cpclient; this package owns flag plumbing and output
// formatting.
package migration

import (
	"github.com/spf13/cobra"
)

// NewCommand returns the `otherix migration` subcommand group, ready to be
// registered onto the root cobra tree by main.
func NewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migration",
		Short: "Inspect VM migrations (CP /v1/migrations surface)",
		Long: `migration groups the read-side operator subcommands against the
Control Plane's /v1/migrations surface — get a migration by id and
list migrations filtered by VM or node. Migrations are *created*
through 'otherix vm migrate <vm>' (a VM sub-resource action), never
here. Authentication and endpoint resolution flow through the
root-level --endpoint / --token / --cluster / --config flags.`,
	}

	cmd.AddCommand(newGetCommand())
	cmd.AddCommand(newListCommand())

	return cmd
}
