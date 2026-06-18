// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package cluster hosts the `otherix cluster` cobra subcommand group.
// The cluster_settings singleton is surfaced via three endpoints —
// GET / PUT / DELETE /v1/cluster/default-pool. This group exposes
// those endpoints via three subcommands:
//
//   - get-default-pool
//   - set-default-pool <name>
//   - unset-default-pool [--force]
//
// All three flow through the standard auth chain (root --token /
// --cluster / --config); the mutating commands require admin and
// surface 403 envelopes verbatim when the caller lacks
// `cluster:manage`.
package cluster

import "github.com/spf13/cobra"

// NewCommand returns the `otherix cluster` subcommand group, ready
// to be registered onto the root cobra tree by main.
func NewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Manage cluster-wide settings (CP /v1/cluster surface)",
		Long: `cluster groups the operator-facing commands against the Control
Plane's cluster-wide settings surface. The first knob exposed is the
default-pool reference: a cluster-wide pool name VM create falls
back to when the request omits --pool.`,
	}
	cmd.AddCommand(newGetDefaultPoolCommand())
	cmd.AddCommand(newSetDefaultPoolCommand())
	cmd.AddCommand(newUnsetDefaultPoolCommand())
	cmd.AddCommand(newGetDefaultArtifactPoolCommand())
	cmd.AddCommand(newSetDefaultArtifactPoolCommand())
	cmd.AddCommand(newUnsetDefaultArtifactPoolCommand())
	cmd.AddCommand(newMemberCommand())
	cmd.AddCommand(newJoinTokenCommand())
	return cmd
}
