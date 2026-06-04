// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package network hosts the `otherix network` cobra subcommand group
// and its children - the CRUD surface (create / list / get / delete)
// over the Control Plane's /v1/networks endpoints.
//
// Networks are cluster-wide bridge / overlay definitions with no owner
// column: every authenticated role can read them (`network:read`),
// admin alone may create or delete them (`network:manage`). Unlike
// storage pools, a network name is globally unique, so there is no
// dual-shape (name vs uuid) view - `network get <name|uuid>` returns
// the same projection either way.
//
// The CP GET-by-id and DELETE routes accept ONLY a UUID literal. To
// keep `get`/`delete <name>` ergonomic for operators, the cpclient
// resolves a name to its UUID client-side via a list-then-match before
// issuing the by-id roundtrip.
package network

import "github.com/spf13/cobra"

// NewCommand returns the `otherix network` subcommand group, ready to
// be registered onto the root cobra tree by main.
func NewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "network",
		Short: "Manage networks (CP /v1/networks surface)",
		Long: `network groups the operator-facing commands against the Control
Plane's /v1/networks surface. Networks are cluster-wide bridge
definitions; every authenticated role can read them, admin alone may
create or delete them. 'network get' surfaces per-node materialisation
status so operators can see how each node reconciled the network.`,
	}
	cmd.AddCommand(newCreateCommand())
	cmd.AddCommand(newListCommand())
	cmd.AddCommand(newGetCommand())
	cmd.AddCommand(newDeleteCommand())
	return cmd
}
