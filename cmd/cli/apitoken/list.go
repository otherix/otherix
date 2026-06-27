// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package apitoken

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// newListCommand returns the `otherix api-token list` cobra command.
// Lists your tokens, or - with --user (admin-only) - another user's.
// Revoked tokens are hidden unless --include-revoked is set.
func newListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List API tokens.",
		Long: `Cursor-paginated list of your API tokens, or - with --user
(admin-only) - another user's. Revoked tokens are hidden unless
--include-revoked is set; an expired-but-not-revoked token is always
shown (status 'expired') so you can see why it stopped working.`,
		RunE: runList,
	}
	cmd.Flags().String(flagUser, "", "act on this user's tokens (admin-only); default: yourself")
	cmd.Flags().Bool(flagIncludeRevoked, false, "include revoked tokens")
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
	username, _ := cmd.Flags().GetString(flagUser)
	includeRevoked, _ := cmd.Flags().GetBool(flagIncludeRevoked)
	limit, _ := cmd.Flags().GetInt(flagLimit)
	cursor, _ := cmd.Flags().GetString(flagCursor)
	format, err := outputFormat(cmd, "table", "yaml")
	if err != nil {
		return err
	}

	userID, err := resolveTargetUserID(cmd.Context(), c, username)
	if err != nil {
		return err
	}

	page, err := c.ListAPITokensFor(cmd.Context(), userID, cpclient.ListAPITokensParams{
		Limit:          limit,
		Cursor:         cursor,
		IncludeRevoked: includeRevoked,
	})
	if err != nil {
		return classifyError(err)
	}

	switch format {
	case "json":
		raw, err := json.MarshalIndent(page, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
	case "yaml":
		out, err := yaml.Marshal(page)
		if err != nil {
			return fmt.Errorf("marshal yaml: %v", err)
		}
		printf(cmd, "%s", out)
	default:
		printTokenTable(cmd, page)
	}
	return nil
}

func printTokenTable(cmd *cobra.Command, page cpclient.APITokenList) {
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "PREFIX\tNAME\tSTATUS\tCREATED_AT\tEXPIRES_AT\tLAST_USED_AT")
	for _, t := range page.Data {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			t.Prefix, t.Name, tokenStatus(t), t.CreatedAt,
			dash(derefOr(t.ExpiresAt, "")), dash(derefOr(t.LastUsedAt, "")))
	}
	_ = tw.Flush()
	if page.Meta.NextCursor != nil {
		printNextCursor(cmd, *page.Meta.NextCursor)
	}
}
