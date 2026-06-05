// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

const (
	flagListPool   = "pool"
	flagListNode   = "node"
	flagListStatus = "status"
	flagListLimit  = "limit"
	flagListCursor = "cursor"
	flagShowIDs    = "show-ids"

	defaultListLimit = 20
)

func newListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List VMs visible to the caller.",
		Long: `Cursor-paginated list. Server-side filters: pool (name or uuid),
node (name or uuid — filters by the agent-reported current location,
not by pinned intent), status (creating/running/paused/stopped/error/
gone/orphaned). The next page's cursor lives in the JSON envelope's
meta.next_cursor and is opaque — re-pass with --cursor.`,
		RunE: runList,
	}

	cmd.Flags().String(flagListPool, "", "filter by storage pool name or uuid")
	cmd.Flags().String(flagListNode, "", "filter by current-location node name or uuid")
	cmd.Flags().String(flagListStatus, "", "filter by status")
	cmd.Flags().Int(flagListLimit, defaultListLimit, "page size (1..200)")
	cmd.Flags().String(flagListCursor, "", "opaque cursor from a previous page")
	cmd.Flags().String(flagOutput, "table", "output format: table|json")
	cmd.Flags().Bool(flagShowIDs, false, "include VM UUIDs in the table output")

	return cmd
}

func runList(cmd *cobra.Command, _ []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}

	pool, err := cmd.Flags().GetString(flagListPool)
	if err != nil {
		return err
	}
	node, err := cmd.Flags().GetString(flagListNode)
	if err != nil {
		return err
	}
	status, err := cmd.Flags().GetString(flagListStatus)
	if err != nil {
		return err
	}
	limit, err := cmd.Flags().GetInt(flagListLimit)
	if err != nil {
		return err
	}
	cursor, err := cmd.Flags().GetString(flagListCursor)
	if err != nil {
		return err
	}
	format, err := outputFormat(cmd, "table")
	if err != nil {
		return err
	}
	showIDs, err := cmd.Flags().GetBool(flagShowIDs)
	if err != nil {
		return err
	}

	vms, err := c.ListVMs(cmd.Context(), cpclient.ListVMsParams{
		Limit:  limit,
		Cursor: cursor,
		Pool:   pool,
		Node:   node,
		Status: status,
	})
	if err != nil {
		return classifyError(err)
	}

	switch format {
	case "json":
		raw, err := json.MarshalIndent(vms, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
	default:
		printVMTable(cmd, vms, showIDs)
	}
	return nil
}

// printVMTable renders an aligned table to stdout. text/tabwriter
// computes column widths once and flushes on Close. The bottom hint
// includes meta.next_cursor when populated so operators can chain
// the next page without digging in JSON. The ID column is hidden by
// default — referenced-resource fields are names, and the VM's own
// UUID is operator-noise unless --show-ids opts back in. The IMAGE
// column carries the image source URL (the template entity is gone).
func printVMTable(cmd *cobra.Command, vms cpclient.VMList, showIDs bool) {
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	if showIDs {
		_, _ = fmt.Fprintln(tw, "ID\tNAME\tSTATUS\tPOOL\tIMAGE")
	} else {
		_, _ = fmt.Fprintln(tw, "NAME\tSTATUS\tPOOL\tIMAGE")
	}
	for _, vm := range vms.Data {
		img := vm.ImageURL
		if showIDs {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
				vm.ID, vm.Name, vm.Status, vm.Pool, img)
		} else {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
				vm.Name, vm.Status, vm.Pool, img)
		}
	}
	_ = tw.Flush()
	if vms.Meta.NextCursor != nil && *vms.Meta.NextCursor != "" {
		printf(cmd, "next_cursor: %s\n", *vms.Meta.NextCursor)
	}
}
