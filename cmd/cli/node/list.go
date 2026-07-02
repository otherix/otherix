// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package node

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

func newListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List cluster nodes.",
		Long: `Cursor-paginated list of nodes. Server-side filters: architecture
(amd64|arm64), status (pending|ready|cordoned|draining|unreachable|
gone). admin / operator callers see the full Node projection;
developer / viewer callers see NodeSummary (no migration capability,
no hardware inventory) — the renderer keeps the table columns
constant and derives CORDONED from the cordoned_at timestamp.`,
		RunE: runList,
	}
	cmd.Flags().String(flagArch, "", "filter by architecture (amd64|arm64)")
	cmd.Flags().String(flagStatus, "", "filter by status")
	cmd.Flags().String(flagRole, "", "filter by role (hypervisor|gateway)")
	cmd.Flags().Int(flagLimit, defaultListLimit, "page size (1..200)")
	cmd.Flags().String(flagCursor, "", "opaque cursor from a previous page")
	cmd.Flags().String(flagOutput, "table", "output format: table|json")
	cmd.Flags().Bool(flagShowIDs, false, "include node UUIDs in the table output")
	return cmd
}

func runList(cmd *cobra.Command, _ []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	arch, _ := cmd.Flags().GetString(flagArch)
	status, _ := cmd.Flags().GetString(flagStatus)
	role, _ := cmd.Flags().GetString(flagRole)
	limit, _ := cmd.Flags().GetInt(flagLimit)
	cursor, _ := cmd.Flags().GetString(flagCursor)
	format, err := outputFormat(cmd, "table")
	if err != nil {
		return err
	}
	showIDs, _ := cmd.Flags().GetBool(flagShowIDs)

	nodes, err := c.ListNodes(cmd.Context(), cpclient.ListNodesParams{
		Limit:        limit,
		Cursor:       cursor,
		Architecture: arch,
		Status:       status,
		Role:         role,
	})
	if err != nil {
		return classifyError(err)
	}

	switch format {
	case "json":
		raw, err := json.MarshalIndent(nodes, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
	default:
		printNodeTable(cmd, nodes, showIDs)
	}
	return nil
}

func printNodeTable(cmd *cobra.Command, nodes cpclient.NodeList, showIDs bool) {
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	if showIDs {
		_, _ = fmt.Fprintln(tw, "ID\tNAME\tROLES\tARCH\tSTATUS\tCORDONED\tAGE")
	} else {
		_, _ = fmt.Fprintln(tw, "NAME\tROLES\tARCH\tSTATUS\tCORDONED\tAGE")
	}
	for _, n := range nodes.Data {
		roles := strings.Join(n.Roles, ",")
		cordoned := boolYesNo(n.CordonedAt != nil)
		status := renderNodeStatus(n)
		age := humanAge(n.CreatedAt)
		if showIDs {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				n.ID, n.Name, roles, n.Architecture, status, cordoned, age)
		} else {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				n.Name, roles, n.Architecture, status, cordoned, age)
		}
	}
	_ = tw.Flush()
	if nodes.Meta.NextCursor != nil {
		printNextCursor(cmd, *nodes.Meta.NextCursor)
	}
}

// renderNodeStatus combines the server-side raw status with any active
// pressure conditions to produce the operator-facing STATUS column
// string. Reachable nodes append "under_pressure" to their status
// when any pressure condition is active. "unreachable" suppresses
// pressure rendering — heartbeat is stale, so pressure data is too.
//
// Renderable combinations:
//
//	ready / cordoned / pending / draining / unreachable / gone
//	under_pressure
//	cordoned, under_pressure
func renderNodeStatus(n cpclient.Node) string {
	if n.Status == "unreachable" {
		return "unreachable"
	}
	if !nodeUnderPressure(n) {
		return n.Status
	}
	if n.Status == "ready" {
		return "under_pressure"
	}
	return n.Status + ", under_pressure"
}

// nodeUnderPressure reports whether any pressure condition is active
// on the node. Node-scoped conditions: memory + system_disk. Pool
// disk pressure is reported on the pool view, not the node — a node
// hosting a pressured pool stays "ready" from the node's perspective.
func nodeUnderPressure(n cpclient.Node) bool {
	return n.MemoryPressure != nil || n.SystemDiskPressure != nil
}
