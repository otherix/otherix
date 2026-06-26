// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package node

import (
	"github.com/spf13/cobra"
)

// newUncordonCommand wires `otherix node uncordon <node>` - return a cordoned
// node to ready so the scheduler resumes placing VMs onto it. The documented
// reverse of a completed drain. Sync (200) and idempotent: uncordoning a ready
// node is a no-op.
func newUncordonCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "uncordon <node>",
		Short: "Resume scheduling onto a cordoned node.",
		Long: `Sends POST /v1/nodes/<node>/uncordon to the Control Plane, returning a
cordoned node to ready so the scheduler resumes placing VMs onto it. This is
the reverse of a completed drain. Idempotent: uncordoning a ready node is a
no-op. Rejected (409) for a node that is pending, draining, unreachable, or
gone.`,
		Args: cobra.ExactArgs(1),
		RunE: runUncordon,
	}
	return cmd
}

func runUncordon(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	node := args[0]
	if err := c.UncordonNode(cmd.Context(), node); err != nil {
		return classifyError(err)
	}
	printf(cmd, "%s: ready\n", node)
	return nil
}
