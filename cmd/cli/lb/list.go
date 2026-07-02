// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package lb

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
		Short: "List load balancers.",
		Long:  `Cursor-paginated list of load balancers.`,
		RunE:  runList,
	}
	cmd.Flags().Int(flagLimit, defaultListLimit, "page size (1..200)")
	cmd.Flags().String(flagCursor, "", "opaque cursor from a previous page")
	cmd.Flags().StringP(flagOutput, "o", "table", "output format: table|json|yaml")
	cmd.Flags().Bool(flagShowIDs, false, "include load balancer UUIDs in the table output")
	return cmd
}

func runList(cmd *cobra.Command, _ []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	limit, _ := cmd.Flags().GetInt(flagLimit)
	cursor, _ := cmd.Flags().GetString(flagCursor)
	format, err := outputFormat(cmd, "table", "yaml")
	if err != nil {
		return err
	}
	showIDs, _ := cmd.Flags().GetBool(flagShowIDs)

	lbs, err := c.ListLoadBalancers(cmd.Context(), cpclient.ListLoadBalancersParams{
		Limit:  limit,
		Cursor: cursor,
	})
	if err != nil {
		return classifyError(err)
	}

	switch format {
	case "json":
		raw, err := json.MarshalIndent(lbs, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
	case "yaml":
		return printLoadBalancerYAML(cmd, lbs)
	default:
		printLoadBalancerTable(cmd, lbs, showIDs)
	}
	return nil
}

// printLoadBalancerYAML projects every load balancer on the page as an
// apply-ready manifest, joined with the `---` separator so the whole page
// round-trips through `create -f`.
func printLoadBalancerYAML(cmd *cobra.Command, lbs cpclient.LoadBalancerList) error {
	docs := make([][]byte, 0, len(lbs.Data))
	for _, lb := range lbs.Data {
		doc, err := manifest.ProjectLoadBalancer(lb)
		if err != nil {
			return err
		}
		docs = append(docs, doc)
	}
	printf(cmd, "%s", manifest.JoinDocuments(docs))
	return nil
}

func printLoadBalancerTable(cmd *cobra.Command, lbs cpclient.LoadBalancerList, showIDs bool) {
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	if showIDs {
		_, _ = fmt.Fprintln(tw, "ID\tNAME\tPORT\tSELECTOR\tSTATUS\tTARGETS")
	} else {
		_, _ = fmt.Fprintln(tw, "NAME\tPORT\tSELECTOR\tSTATUS\tTARGETS")
	}
	for _, lb := range lbs.Data {
		status, targets := lbHealthColumns(lb.Health)
		if showIDs {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n", lb.ID, lb.Name, lb.Port, formatSelector(lb.Selector), status, targets)
		} else {
			_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", lb.Name, lb.Port, formatSelector(lb.Selector), status, targets)
		}
	}
	_ = tw.Flush()
	if lbs.Meta.NextCursor != nil {
		printNextCursor(cmd, *lbs.Meta.NextCursor)
	}
}

// lbHealthColumns renders the STATUS and TARGETS cells for a load balancer's
// aggregate health summary, or "-"/"-" when the CP attached none (e.g. it could
// not resolve the owner's backends for this row).
func lbHealthColumns(h *cpclient.LoadBalancerHealthSummary) (status, targets string) {
	if h == nil {
		return "-", "-"
	}
	return h.Status, fmt.Sprintf("%d/%d", h.TargetsHealthy, h.TargetsTotal)
}
