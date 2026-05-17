// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package node hosts the `otherix node` cobra subcommand group и its
// children. Read-only discovery surface для now; admin-level node
// management (create / cordon / uncordon / delete) lives through the
// REST API directly, with CLI verbs planned for a later iteration.
package node

import "github.com/spf13/cobra"

// NewCommand returns the `otherix node` subcommand group, ready к
// be registered onto the root cobra tree by main.
func NewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "node",
		Short: "Browse cluster nodes (CP /v1/nodes surface)",
		Long: `node groups the operator-facing read commands против the Control
Plane's /v1/nodes surface. admin / operator callers see the full
Node projection (including migration capability и hardware
inventory); developer / viewer callers see the reduced NodeSummary
shape — both shapes decode into the same client struct, with
admin-only fields left nil on the lighter projection.`,
	}
	cmd.AddCommand(newListCommand())
	cmd.AddCommand(newGetCommand())
	cmd.AddCommand(newJoinTokenCommand())
	return cmd
}
