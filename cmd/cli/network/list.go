// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package network

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
	"github.com/otherix/otherix/cmd/cli/internal/manifest"
)

func newListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List networks.",
		Long: `Cursor-paginated list of cluster-wide networks. Optional server
filter: --type (only the 'bridge' value is meaningful today). Every
authenticated role may list networks.`,
		RunE: runList,
	}
	cmd.Flags().String(flagType, "", "filter by network type (bridge)")
	cmd.Flags().Int(flagLimit, defaultListLimit, "page size (1..200)")
	cmd.Flags().String(flagCursor, "", "opaque cursor from a previous page")
	cmd.Flags().StringP(flagOutput, "o", "table", "output format: table|json|yaml")
	cmd.Flags().Bool(flagShowIDs, false, "include network UUIDs in the table output")
	return cmd
}

func runList(cmd *cobra.Command, _ []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	netType, _ := cmd.Flags().GetString(flagType)
	limit, _ := cmd.Flags().GetInt(flagLimit)
	cursor, _ := cmd.Flags().GetString(flagCursor)
	format, err := outputFormat(cmd, "table")
	if err != nil {
		return err
	}
	showIDs, _ := cmd.Flags().GetBool(flagShowIDs)

	networks, err := c.ListNetworks(cmd.Context(), cpclient.ListNetworksParams{
		Type:   netType,
		Limit:  limit,
		Cursor: cursor,
	})
	if err != nil {
		return classifyError(err)
	}

	if format == "yaml" {
		docs := make([][]byte, 0, len(networks.Data))
		for _, n := range networks.Data {
			out, err := manifest.ProjectNetwork(n)
			if err != nil {
				return err
			}
			docs = append(docs, out)
		}
		printf(cmd, "%s", manifest.JoinDocuments(docs))
		return nil
	}

	switch format {
	case "json":
		raw, err := json.MarshalIndent(networks, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
	default:
		printNetworkTable(cmd, networks, showIDs)
	}
	return nil
}

func printNetworkTable(cmd *cobra.Command, networks cpclient.NetworkList, showIDs bool) {
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	if showIDs {
		_, _ = fmt.Fprintln(tw, "ID\tNAME\tTYPE\tBRIDGE\tMANAGED\tEGRESS\tMTU")
	} else {
		_, _ = fmt.Fprintln(tw, "NAME\tTYPE\tBRIDGE\tMANAGED\tEGRESS\tMTU")
	}
	for _, n := range networks.Data {
		managed := boolYesNo(n.Managed)
		if showIDs {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\n",
				n.ID, n.Name, n.Type, n.BridgeName, managed, n.Egress, n.MTU)
		} else {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\n",
				n.Name, n.Type, n.BridgeName, managed, n.Egress, n.MTU)
		}
	}
	_ = tw.Flush()
	if networks.Meta.NextCursor != nil && *networks.Meta.NextCursor != "" {
		printf(cmd, "next_cursor: %s\n", *networks.Meta.NextCursor)
	}
}
