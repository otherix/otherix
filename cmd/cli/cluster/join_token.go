// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cluster

import (
	"time"

	"github.com/spf13/cobra"
)

// Local flag names for the cluster join-token subcommand group.
// flagOutput is inherited from common.go.
const (
	flagTTL         = "ttl"
	flagMaxUses     = "max-uses"
	defaultTokenTTL = time.Hour
)

// newJoinTokenCommand returns the `otherix cluster join-token` subgroup.
// Its single child mints kind=cluster tokens for growing a single-node
// control plane to HA. Listing / revoking cluster tokens is covered by
// the kind-agnostic `otherix node join-token list|revoke` commands (the
// tokens share one table), so this group ships create only.
func newJoinTokenCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "join-token",
		Short: "Issue cluster-replica join tokens (HA grow).",
		Long: `join-token mints the kind=cluster tokens an existing replica hands
to a new otherix-api so it can join the etcd cluster. A cluster token
redeems for the cluster CA key, so it is single-use by default.

Requires admin (node:manage).`,
	}
	cmd.AddCommand(newJoinTokenCreateCommand())
	return cmd
}
