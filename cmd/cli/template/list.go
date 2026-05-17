// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package template

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
		Short: "List templates visible к the caller.",
		Long: `Cursor-paginated list of templates. Server-side visibility scoping is
applied per the caller's role (admin/operator see every template;
developer sees public + own private; viewer sees public only). Optional
filters: visibility, architecture, os_family. The next page's cursor
lives в the JSON envelope's meta.next_cursor and is opaque —
re-pass с --cursor.`,
		RunE: runList,
	}
	cmd.Flags().String(flagVisible, "", "filter by visibility (public|private)")
	cmd.Flags().String(flagArch, "", "filter by architecture (amd64|arm64)")
	cmd.Flags().String(flagOSFamily, "", "filter by os_family (linux|windows)")
	cmd.Flags().Int(flagLimit, defaultListLimit, "page size (1..200)")
	cmd.Flags().String(flagCursor, "", "opaque cursor from a previous page")
	cmd.Flags().String(flagOutput, "table", "output format: table|json")
	cmd.Flags().Bool(flagShowIDs, false, "include template UUIDs in the table output")
	return cmd
}

func runList(cmd *cobra.Command, _ []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	vis, _ := cmd.Flags().GetString(flagVisible)
	arch, _ := cmd.Flags().GetString(flagArch)
	osFamily, _ := cmd.Flags().GetString(flagOSFamily)
	limit, _ := cmd.Flags().GetInt(flagLimit)
	cursor, _ := cmd.Flags().GetString(flagCursor)
	format, err := outputFormat(cmd, "table")
	if err != nil {
		return err
	}
	showIDs, _ := cmd.Flags().GetBool(flagShowIDs)

	templates, err := c.ListTemplates(cmd.Context(), cpclient.ListTemplatesParams{
		Limit:        limit,
		Cursor:       cursor,
		Visibility:   vis,
		Architecture: arch,
		OSFamily:     osFamily,
	})
	if err != nil {
		return classifyError(err)
	}

	switch format {
	case "json":
		raw, err := json.MarshalIndent(templates, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
	default:
		printTemplateTable(cmd, templates, showIDs)
	}
	return nil
}

func printTemplateTable(cmd *cobra.Command, templates cpclient.TemplateList, showIDs bool) {
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	if showIDs {
		_, _ = fmt.Fprintln(tw, "ID\tNAME\tARCHITECTURE\tOS_FAMILY\tVISIBILITY\tAGE")
	} else {
		_, _ = fmt.Fprintln(tw, "NAME\tARCHITECTURE\tOS_FAMILY\tVISIBILITY\tAGE")
	}
	for _, t := range templates.Data {
		age := humanAge(t.CreatedAt)
		if showIDs {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				t.ID, t.Name, t.Architecture, t.OSFamily, t.Visibility, age)
		} else {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
				t.Name, t.Architecture, t.OSFamily, t.Visibility, age)
		}
	}
	_ = tw.Flush()
	if templates.Meta.NextCursor != nil && *templates.Meta.NextCursor != "" {
		printf(cmd, "next_cursor: %s\n", *templates.Meta.NextCursor)
	}
}
