// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package node

import (
	"github.com/spf13/cobra"
)

// newReadmitCommand wires `otherix node readmit <node>` - return a node stuck in
// the terminal gone status to pending so the cluster re-accepts its heartbeats.
// Sync (200) and idempotent: readmitting a pending node is a no-op. Rejected
// (409) for a node that is ready, cordoned, draining, or unreachable.
func newReadmitCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "readmit <node>",
		Short: "Recover a node from the terminal gone status.",
		Long: `Sends POST /v1/nodes/<node>/readmit to the Control Plane, returning a
node stuck in the terminal gone status to pending so the cluster re-accepts its
heartbeats; the promotion path advances it back to ready on the next fresh
heartbeat. Idempotent: readmitting a pending node is a no-op. Rejected (409) for
a node that is ready, cordoned, draining, or unreachable.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFromFlags(cmd)
			if err != nil {
				return err
			}
			node := args[0]
			if err := c.ReadmitNode(cmd.Context(), node); err != nil {
				return classifyError(err)
			}
			printf(cmd, "%s: readmitted (pending)\n", node)
			return nil
		},
	}
	return cmd
}
