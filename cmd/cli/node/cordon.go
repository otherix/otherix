// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package node

import (
	"github.com/spf13/cobra"
)

// newCordonCommand wires `otherix node cordon <node>` - mark a node cordoned so
// the scheduler stops placing new VMs onto it. Sync (200) and idempotent:
// cordoning an already-cordoned node is a no-op.
func newCordonCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cordon <node>",
		Short: "Stop the scheduler placing new VMs onto a node.",
		Long: `Sends POST /v1/nodes/<node>/cordon to the Control Plane. Cordoning a
node keeps its running VMs but stops the scheduler placing new ones onto it.
Idempotent: cordoning an already-cordoned node is a no-op. Rejected (409) for
a node that is gone or already draining.`,
		Args: cobra.ExactArgs(1),
		RunE: runCordon,
	}
	return cmd
}

func runCordon(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	node := args[0]
	if err := c.CordonNode(cmd.Context(), node); err != nil {
		return classifyError(err)
	}
	printf(cmd, "%s: cordoned\n", node)
	return nil
}
