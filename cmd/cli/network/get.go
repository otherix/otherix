// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package network

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

func newGetCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <name|uuid>",
		Short: "Show a network and its per-node materialisation status.",
		Long: `Fetches a single network. The positional accepts a network name or
a UUID literal. The CP GET-by-id route accepts only a UUID, so a name
is resolved to its UUID client-side (the name is globally unique).

The text output includes a STATUS section listing each node that has
reported a reconciliation outcome for the network (NODE, STATUS,
ERROR), so operators can see how the network materialised per node.`,
		Args: cobra.ExactArgs(1),
		RunE: runGet,
	}
	cmd.Flags().String(flagOutput, "text", "output format: text|json")
	return cmd
}

func runGet(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	identifier := args[0]
	format, err := outputFormat(cmd, "text")
	if err != nil {
		return err
	}

	net, err := c.GetNetwork(cmd.Context(), identifier)
	if err != nil {
		return classifyError(err)
	}
	return renderGet(cmd, net, format)
}

func renderGet(cmd *cobra.Command, n cpclient.Network, format string) error {
	if format == "json" {
		raw, err := json.MarshalIndent(n, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
		return nil
	}
	printf(cmd, "id: %s\n", n.ID)
	printf(cmd, "name: %s\n", n.Name)
	printf(cmd, "type: %s\n", n.Type)
	printf(cmd, "bridge_name: %s\n", n.BridgeName)
	printf(cmd, "managed: %t\n", n.Managed)
	printf(cmd, "egress: %s\n", n.Egress)
	printf(cmd, "subnet: %s\n", orDash(n.Subnet))
	printf(cmd, "gateway: %s\n", orDash(n.Gateway))
	printf(cmd, "mtu: %d\n", n.MTU)
	if n.VlanTag != nil {
		printf(cmd, "vlan_tag: %d\n", *n.VlanTag)
	}
	printf(cmd, "created_at: %s\n", n.CreatedAt)
	printf(cmd, "updated_at: %s\n", n.UpdatedAt)
	printNetworkStatus(cmd, n.Status)
	return nil
}

// printNetworkStatus renders the per-node materialisation rollup. When
// no node has reported yet (or the rollup is absent), it prints an
// explicit "<none>" so the operator knows the section exists but is
// empty rather than wondering whether it was omitted.
func printNetworkStatus(cmd *cobra.Command, status *cpclient.NetworkStatus) {
	printf(cmd, "status:\n")
	if status == nil || len(status.Nodes) == 0 {
		printf(cmd, "  <none>\n")
		return
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "  NODE\tSTATUS\tERROR")
	for _, node := range status.Nodes {
		errMsg := "-"
		if node.ReconciliationError != nil && *node.ReconciliationError != "" {
			errMsg = *node.ReconciliationError
		}
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\n", node.NodeID, node.ReconciliationStatus, errMsg)
	}
	_ = tw.Flush()
}
