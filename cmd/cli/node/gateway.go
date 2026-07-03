// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package node

import (
	"github.com/spf13/cobra"
)

// newGatewayCommand groups `otherix node gateway enable|disable <node>` - the
// admin verbs that assign or remove the ingress-gateway role on a node. Both
// children are sync (200) and idempotent: enabling a node that already carries
// the role, or disabling one that does not, is a no-op.
func newGatewayCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gateway",
		Short: "Assign or remove the ingress-gateway role on a node.",
	}
	cmd.AddCommand(newGatewayEnableCommand())
	cmd.AddCommand(newGatewayDisableCommand())
	return cmd
}

func newGatewayEnableCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "enable <node>",
		Short: "Assign the ingress-gateway role to a node.",
		Long: `Sends POST /v1/nodes/<node>/gateway/enable to the Control Plane,
assigning the ingress-gateway role so the node can front external ingress
traffic. Idempotent: enabling a node that already carries the role is a
no-op.`,
		Args: cobra.ExactArgs(1),
		RunE: runGatewayEnable,
	}
}

func runGatewayEnable(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	node := args[0]
	if err := c.EnableGateway(cmd.Context(), node); err != nil {
		return classifyError(err)
	}
	printf(cmd, "%s: gateway enabled\n", node)
	return nil
}

func newGatewayDisableCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "disable <node>",
		Short: "Remove the ingress-gateway role from a node.",
		Long: `Sends POST /v1/nodes/<node>/gateway/disable to the Control Plane,
removing the ingress-gateway role from the node. Idempotent: disabling a node
that does not carry the role is a no-op.`,
		Args: cobra.ExactArgs(1),
		RunE: runGatewayDisable,
	}
}

func runGatewayDisable(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	node := args[0]
	if err := c.DisableGateway(cmd.Context(), node); err != nil {
		return classifyError(err)
	}
	printf(cmd, "%s: gateway disabled\n", node)
	return nil
}
