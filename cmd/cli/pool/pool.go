// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package pool hosts the `otherix pool` cobra subcommand group and its
// children — full CRUD surface (list / get / create / delete) over the
// Control Plane's /v1/storage-pools endpoints.
//
// Multi-instance reality is the central UX claim here: the same pool
// name can live on multiple nodes, and `pool get <name>` returns the
// aggregated PoolConceptView (StorageClass mental model), while
// `pool get <uuid>` returns the flat per-node instance
// (PersistentVolume mental model). The CLI parses the positional
// locally to pick which roundtrip to make — the server is polymorphic
// but a name input cannot return the flat shape, so the local fork
// avoids an unintended aggregated-view fetch on UUID inputs.
//
// 'pool create' and 'pool delete' operate on single (name, node)
// instances per invocation. Multi-instance fan-out is achieved by
// re-running the command per node; no batch shorthand is offered to
// keep per-call semantics observable.
package pool

import "github.com/spf13/cobra"

// NewCommand returns the `otherix pool` subcommand group, ready to be
// registered onto the root cobra tree by main.
func NewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pool",
		Short: "Manage storage pools",
		Long: `pool groups the operator-facing commands against the Control Plane's
/v1/storage-pools surface. Multi-instance: the same pool name may
live on multiple nodes; 'pool list' shows every per-node instance, and
'pool get <name>' aggregates them into a single cluster-wide view.
'pool create' and 'pool delete' operate on single (name, node)
instances per invocation — admin-only.`,
	}
	cmd.AddCommand(newListCommand())
	cmd.AddCommand(newGetCommand())
	cmd.AddCommand(newCreateCommand())
	cmd.AddCommand(newDeleteCommand())
	return cmd
}
