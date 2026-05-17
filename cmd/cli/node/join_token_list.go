// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package node

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// newJoinTokenListCommand builds `otherix node join-token list`.
func newJoinTokenListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List join tokens (admin only).",
		Long: `Cursor-paginated list of join tokens. Expired tokens are excluded
by default — pass --include-expired к surface them. Output is а
human-friendly table (Q20 lock columns: ID | NODE-NAME |
TTL-REMAINING | MAX-USES | CONSUMED | CREATED-BY) или JSON for
scripting.`,
		RunE: runJoinTokenList,
	}
	cmd.Flags().Bool(flagIncExpired, false, "surface expired tokens")
	cmd.Flags().Int(flagLimit, defaultListLimit, "page size (1..200)")
	cmd.Flags().String(flagCursor, "", "opaque cursor from а previous page")
	cmd.Flags().String(flagOutput, "table", "output format: table|json")
	return cmd
}

func runJoinTokenList(cmd *cobra.Command, _ []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	includeExpired, _ := cmd.Flags().GetBool(flagIncExpired)
	limit, _ := cmd.Flags().GetInt(flagLimit)
	cursor, _ := cmd.Flags().GetString(flagCursor)
	format, err := outputFormat(cmd, "table")
	if err != nil {
		return err
	}

	tokens, err := c.ListJoinTokens(cmd.Context(), cpclient.ListJoinTokensParams{
		Limit:          limit,
		Cursor:         cursor,
		IncludeExpired: includeExpired,
	})
	if err != nil {
		return classifyError(err)
	}

	switch format {
	case "json":
		raw, err := json.MarshalIndent(tokens, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
	default:
		printJoinTokenTable(cmd, tokens)
	}
	return nil
}

func printJoinTokenTable(cmd *cobra.Command, tokens cpclient.JoinTokenList) {
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tNODE-NAME\tTTL-REMAINING\tMAX-USES\tCONSUMED\tCREATED-BY")
	for _, t := range tokens.Data {
		createdBy := "-"
		if t.CreatedByUserID != nil {
			createdBy = t.CreatedByUserID.String()
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n",
			t.ID,
			nodeNameLabel(t.IntendedNodeName),
			humanTTLRemaining(t.ExpiresAt),
			maxUsesLabel(t.MaxUses),
			t.ConsumptionCount,
			createdBy,
		)
	}
	_ = tw.Flush()
	if tokens.Meta.NextCursor != nil && *tokens.Meta.NextCursor != "" {
		printf(cmd, "next_cursor: %s\n", *tokens.Meta.NextCursor)
	}
}
