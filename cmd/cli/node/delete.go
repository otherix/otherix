// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package node

import (
	"github.com/spf13/cobra"
)

// newDeleteCommand wires `otherix node delete <node> [--force]` - decommission a
// node. Without --force the Control Plane refuses (409) a node that still hosts
// VMs or active migrations; --force cancels those migrations and orphans the VM
// runtime rows before soft-deleting the node. The node's agent cert is revoked
// either way.
func newDeleteCommand() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "delete <node>",
		Short: "Delete a cluster node.",
		Long: `Sends DELETE /v1/nodes/<node> to the Control Plane. Without --force the
request is refused (409) while the node still hosts VMs or active migrations;
re-run with --force to cancel those migrations and orphan the VM runtime rows.
The node's agent cert is revoked on delete.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFromFlags(cmd)
			if err != nil {
				return err
			}
			node := args[0]
			if err := c.DeleteNode(cmd.Context(), node, force); err != nil {
				return classifyError(err)
			}
			printf(cmd, "%s: deleted\n", node)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "cancel active migrations and orphan VM runtime rows, then delete")
	return cmd
}
