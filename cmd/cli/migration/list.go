// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package migration

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

func newListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List migrations visible to the caller.",
		Long: `Cursor-paginated list, newest-first. Server-side filters: --vm (a VM
UUID — migrations for that VM) and --node (a node UUID — migrations
touching that node as source or target). The next page's cursor is
printed below the table and is opaque — re-pass with --cursor.`,
		RunE: runList,
	}
	cmd.Flags().String(flagVM, "", "filter by VM uuid")
	cmd.Flags().String(flagNode, "", "filter by node uuid (source or target)")
	cmd.Flags().Int(flagLimit, defaultListLimit, "page size (1..200)")
	cmd.Flags().String(flagCursor, "", "opaque cursor from a previous page")
	cmd.Flags().StringP(flagOutput, "o", "table", "output format: table|json|yaml")
	return cmd
}

func runList(cmd *cobra.Command, _ []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}

	vmFilter, err := cmd.Flags().GetString(flagVM)
	if err != nil {
		return err
	}
	nodeFilter, err := cmd.Flags().GetString(flagNode)
	if err != nil {
		return err
	}
	limit, err := cmd.Flags().GetInt(flagLimit)
	if err != nil {
		return err
	}
	cursor, err := cmd.Flags().GetString(flagCursor)
	if err != nil {
		return err
	}
	format, err := outputFormat(cmd, "table")
	if err != nil {
		return err
	}

	migrations, next, err := c.ListMigrations(cmd.Context(), cpclient.ListMigrationsParams{
		VM:     vmFilter,
		Node:   nodeFilter,
		Limit:  limit,
		Cursor: cursor,
	})
	if err != nil {
		return classifyError(err)
	}

	switch format {
	case "json", "yaml":
		envelope := struct {
			Data []cpclient.Migration `json:"data"`
			Meta struct {
				NextCursor *string `json:"next_cursor"`
			} `json:"meta"`
		}{Data: migrations}
		if next != "" {
			envelope.Meta.NextCursor = &next
		}
		raw, mErr := json.MarshalIndent(envelope, "", "  ")
		if mErr != nil {
			return fmt.Errorf("marshal json: %v", mErr)
		}
		printf(cmd, "%s\n", raw)
	default:
		printMigrationTable(cmd, migrations, next)
	}
	return nil
}

// printMigrationTable renders an aligned table to stdout. Columns: ID VM SOURCE
// TARGET PHASE PROGRESS AGE — ID is the short migration id (the CP resolves the
// prefix back), VM/SOURCE/TARGET show human names (falling back to the id, then
// "-", for a node-less / not-yet-scheduled / force-deleted-node migration). The
// bottom hint prints next_cursor when populated so operators can chain the next
// page. Full ids remain available via --output json.
func printMigrationTable(cmd *cobra.Command, migrations []cpclient.Migration, next string) {
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tVM\tSOURCE\tTARGET\tPHASE\tPROGRESS\tAGE")
	for _, m := range migrations {
		progress := fmt.Sprintf("%d%%", m.ProgressPercent)
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			shortID(m.ID), vmLabel(m),
			nodeLabel(m.SourceNodeName, m.SourceNodeID),
			nodeLabel(m.TargetNodeName, m.TargetNodeID),
			m.Phase, progress, humanAge(m.CreatedAt))
	}
	_ = tw.Flush()
	if next != "" {
		printf(cmd, "next_cursor: %s\n", next)
	}
}
