// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package artifactpool hosts the `otherix artifact-pool` cobra subcommands -
// create / get / list / delete against the CP /v1/artifact-pools surface.
//
// Artifact pools are cluster-level content-addressed stores for snapshots
// (and, later, images) with no owner column: every authenticated role can
// read them (storage_pool:read), admin alone may create or delete them
// (storage_pool:manage). The CP GET-by-id and DELETE routes accept a UUID OR
// a name literal and resolve it server-side, so the CLI passes the operator's
// identifier through verbatim - no client-side name resolution.
package artifactpool

import "github.com/spf13/cobra"

// NewCommand returns the `otherix artifact-pool` subcommand group, ready to be
// registered onto the root cobra tree by main.
func NewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "artifact-pool",
		Short: "Manage cluster-level artifact pools (CP /v1/artifact-pools surface)",
		Long: `artifact-pool groups the operator-facing commands against the Control
Plane's /v1/artifact-pools surface. Artifact pools are cluster-level
content-addressed stores for snapshots (and, later, images); every
authenticated role can read them, admin alone may create or delete them.`,
	}
	cmd.AddCommand(newCreateCommand())
	cmd.AddCommand(newGetCommand())
	cmd.AddCommand(newUpdateCommand())
	cmd.AddCommand(newListCommand())
	cmd.AddCommand(newDeleteCommand())
	return cmd
}
