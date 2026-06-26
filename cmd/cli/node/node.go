// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package node hosts the `otherix node` cobra subcommand group and its
// children: the read-only discovery surface (list / get) plus the admin-level
// maintenance verbs drain / cordon / uncordon.
package node

import "github.com/spf13/cobra"

// NewCommand returns the `otherix node` subcommand group, ready to
// be registered onto the root cobra tree by main.
func NewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "node",
		Short: "Browse cluster nodes (CP /v1/nodes surface)",
		Long: `node groups the operator-facing read commands against the Control
Plane's /v1/nodes surface. admin / operator callers see the full
Node projection (including migration capability and hardware
inventory); developer / viewer callers see the reduced NodeSummary
shape — both shapes decode into the same client struct, with
admin-only fields left nil on the lighter projection.`,
	}
	cmd.AddCommand(newListCommand())
	cmd.AddCommand(newGetCommand())
	cmd.AddCommand(newDrainCommand())
	cmd.AddCommand(newCordonCommand())
	cmd.AddCommand(newUncordonCommand())
	cmd.AddCommand(newJoinTokenCommand())
	return cmd
}
