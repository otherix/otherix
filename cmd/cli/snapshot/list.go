// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package snapshot

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
		Short: "List snapshots visible to the caller.",
		Long: `Cursor-paginated list, newest-first, across every VM. Server-side
filters: --vm (a VM UUID - snapshots of that VM) and --status (the snapshot
status enum). The next page's cursor is printed below the table and is opaque -
re-pass with --cursor. Full ids and the per-disk manifest remain available via
--output json.`,
		RunE: runList,
	}
	cmd.Flags().String(flagVM, "", "filter by VM uuid")
	cmd.Flags().String(flagStatus, "", "filter by snapshot status")
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
	statusFilter, err := cmd.Flags().GetString(flagStatus)
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

	snapshots, next, err := c.ListSnapshotsGlobal(cmd.Context(), cpclient.ListSnapshotsParams{
		VM:     vmFilter,
		Status: statusFilter,
		Limit:  limit,
		Cursor: cursor,
	})
	if err != nil {
		return classifyError(err)
	}

	switch format {
	case "json", "yaml":
		envelope := struct {
			Data []cpclient.SnapshotListItem `json:"data"`
			Meta struct {
				NextCursor *string `json:"next_cursor"`
			} `json:"meta"`
		}{Data: snapshots}
		if next != "" {
			envelope.Meta.NextCursor = &next
		}
		raw, mErr := json.MarshalIndent(envelope, "", "  ")
		if mErr != nil {
			return fmt.Errorf("marshal json: %v", mErr)
		}
		printf(cmd, "%s\n", raw)
	default:
		printSnapshotTable(cmd, snapshots, next)
	}
	return nil
}

// printSnapshotTable renders an aligned table to stdout. Columns:
// ID VM NAME STATUS OWNER SIZE AGE - ID is the short snapshot id (the full
// UUID is in the json path), VM is the resolved vm_name, OWNER is the resolved
// owner_display_name (both "-" when their row is gone), SIZE is the summed blob
// size ("-" until measured), AGE is the snapshot's age. The bottom hint prints
// next_cursor when populated so operators can chain the next page.
func printSnapshotTable(cmd *cobra.Command, snapshots []cpclient.SnapshotListItem, next string) {
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tVM\tNAME\tSTATUS\tOWNER\tSIZE\tAGE")
	for _, s := range snapshots {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			shortID(s.ID), dashIfNil(s.VMName), s.Name, s.Status,
			dashIfNil(s.OwnerDisplayName), snapshotSize(s.DiskSizeBytes), humanAge(s.CreatedAt))
	}
	_ = tw.Flush()
	printNextCursor(cmd, next)
}
