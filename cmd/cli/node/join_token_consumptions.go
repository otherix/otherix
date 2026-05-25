// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package node

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// newJoinTokenConsumptionsCommand builds `otherix node join-token
// consumptions <id>` — audit trail surface.
func newJoinTokenConsumptionsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "consumptions <id>",
		Short: "List consumption audit entries for a join token (admin only).",
		Long: `Cursor-paginated audit trail of agent bootstrap events that
consumed the token. Empty in Step 1 (redemption endpoint lands in
Step 2 of the bootstrap rollout).`,
		Args: cobra.ExactArgs(1),
		RunE: runJoinTokenConsumptions,
	}
	cmd.Flags().Int(flagLimit, defaultListLimit, "page size (1..200)")
	cmd.Flags().String(flagCursor, "", "opaque cursor from a previous page")
	cmd.Flags().String(flagOutput, "table", "output format: table|json")
	return cmd
}

func runJoinTokenConsumptions(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	id, err := uuid.Parse(args[0])
	if err != nil {
		return fmt.Errorf("validation_failed: id must be a uuid: %v", err)
	}
	limit, _ := cmd.Flags().GetInt(flagLimit)
	cursor, _ := cmd.Flags().GetString(flagCursor)
	format, err := outputFormat(cmd, "table")
	if err != nil {
		return err
	}

	list, err := c.ListJoinTokenConsumptions(cmd.Context(), id, cpclient.ListJoinTokenConsumptionsParams{
		Limit:  limit,
		Cursor: cursor,
	})
	if err != nil {
		return classifyError(err)
	}

	switch format {
	case "json":
		raw, err := json.MarshalIndent(list, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
	default:
		printConsumptionsTable(cmd, list)
	}
	return nil
}

func printConsumptionsTable(cmd *cobra.Command, list cpclient.JoinTokenConsumptionList) {
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tNODE-ID\tCONSUMED-AT\tSOURCE-IP")
	for _, c := range list.Data {
		nodeID := "-"
		if c.ConsumedByNodeID != nil {
			nodeID = c.ConsumedByNodeID.String()
		}
		sourceIP := "-"
		if c.SourceIP != nil {
			sourceIP = *c.SourceIP
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", c.ID, nodeID, c.ConsumedAt, sourceIP)
	}
	_ = tw.Flush()
	if list.Meta.NextCursor != nil && *list.Meta.NextCursor != "" {
		printf(cmd, "next_cursor: %s\n", *list.Meta.NextCursor)
	}
}
