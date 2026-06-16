// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package pool

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
		Short: "List storage pools.",
		Long: `Cursor-paginated list of storage pool instances. Multi-instance:
the same pool name may appear multiple times — one row per node. The
DEFAULT column reflects cluster_settings.default_pool_name (set via
'otherix cluster set-default-pool'). Optional server filters:
--node (name or uuid), --type.`,
		RunE: runList,
	}
	cmd.Flags().String(flagNode, "", "filter by owning node name or uuid")
	cmd.Flags().String(flagType, "", "filter by pool type (local_dir)")
	cmd.Flags().Int(flagLimit, defaultListLimit, "page size (1..200)")
	cmd.Flags().String(flagCursor, "", "opaque cursor from a previous page")
	cmd.Flags().StringP(flagOutput, "o", "table", "output format: table|json|yaml")
	cmd.Flags().Bool(flagShowIDs, false, "include pool instance UUIDs in the table output")
	return cmd
}

func runList(cmd *cobra.Command, _ []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	node, _ := cmd.Flags().GetString(flagNode)
	poolType, _ := cmd.Flags().GetString(flagType)
	limit, _ := cmd.Flags().GetInt(flagLimit)
	cursor, _ := cmd.Flags().GetString(flagCursor)
	format, err := outputFormat(cmd, "table", "yaml")
	if err != nil {
		return err
	}
	showIDs, _ := cmd.Flags().GetBool(flagShowIDs)

	pools, err := c.ListPools(cmd.Context(), cpclient.ListPoolsParams{
		Limit:  limit,
		Cursor: cursor,
		Node:   node,
		Type:   poolType,
	})
	if err != nil {
		return classifyError(err)
	}

	if format == "yaml" {
		docs := make([][]byte, 0, len(pools.Data))
		for _, p := range pools.Data {
			out, err := manifest.ProjectPoolInstance(p)
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
		raw, err := json.MarshalIndent(pools, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
	default:
		printPoolTable(cmd, pools, showIDs)
	}
	return nil
}

func printPoolTable(cmd *cobra.Command, pools cpclient.PoolList, showIDs bool) {
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	if showIDs {
		_, _ = fmt.Fprintln(tw, "ID\tNAME\tNODE\tTYPE\tPATH\tAVAILABLE\tSTATUS\tDEFAULT\tAGE")
	} else {
		_, _ = fmt.Fprintln(tw, "NAME\tNODE\tTYPE\tPATH\tAVAILABLE\tSTATUS\tDEFAULT\tAGE")
	}
	for _, p := range pools.Data {
		avail := formatPoolAvailable(p.AvailableBytes, p.AvailableBytesEffective)
		status := renderPoolStatus(p)
		def := boolYesNo(p.IsClusterDefault)
		age := humanAge(p.CreatedAt)
		if showIDs {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				p.ID, p.Name, p.Node, p.Type, p.Path, avail, status, def, age)
		} else {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				p.Name, p.Node, p.Type, p.Path, avail, status, def, age)
		}
	}
	_ = tw.Flush()
	if pools.Meta.NextCursor != nil {
		printNextCursor(cmd, *pools.Meta.NextCursor)
	}
}

// renderPoolStatus computes the operator-facing STATUS column for a
// pool row using a worst-of priority order that composes node-level
// and pool-level conditions into a single label:
//
//	failed > unreachable > cordoned > under_pressure > pending > ready
//
// Rationale: the STATUS column is a single-cell summary, so the most
// actionable condition wins. Reconciliation `failed` outranks
// `unreachable` because a recorded failure is operator-actionable
// (read the error, fix the path) even via a dead node; once the
// node returns the row stays failed until reconciled. Node-level
// `unreachable` outranks operator-driven `cordoned` because lost
// connectivity invalidates other conditions (pressure data goes
// stale). `under_pressure` (disk_pressure on this pool) outranks
// `pending` because pressure has placement consequences and pending
// is transitional. `ready` is the all-clear path.
//
// Other node statuses (`draining`, `gone`) and a nil node_status
// (orphaned pool — owning node soft-deleted) fall through to the
// pool-level checks; operators can dig via `otherix node get` if
// they need the raw lifecycle value.
//
// No new conditions are added — only existing axes (`disk_pressure`,
// `nodes.status`, `reconciliation_status`) are composed.
func renderPoolStatus(p cpclient.Pool) string {
	if p.ReconciliationStatus == "failed" {
		return "failed"
	}
	if p.NodeStatus != nil {
		switch *p.NodeStatus {
		case "unreachable":
			return "unreachable"
		case "cordoned":
			return "cordoned"
		}
	}
	if p.DiskPressure != nil {
		return "under_pressure"
	}
	if p.ReconciliationStatus == "pending" {
		return "pending"
	}
	return "ready"
}
