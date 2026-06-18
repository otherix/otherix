// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package artifactpool

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

func newListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List cluster-level artifact pools.",
		Long: `Cursor-paginated list of cluster-level artifact pools. Every
authenticated role may list them.`,
		RunE: runList,
	}
	cmd.Flags().Int(flagLimit, defaultListLimit, "page size (1..200)")
	cmd.Flags().String(flagCursor, "", "opaque cursor from a previous page")
	cmd.Flags().StringP(flagOutput, "o", "table", "output format: table|json")
	return cmd
}

func runList(cmd *cobra.Command, _ []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	limit, _ := cmd.Flags().GetInt(flagLimit)
	cursor, _ := cmd.Flags().GetString(flagCursor)
	format, err := outputFormat(cmd, "table")
	if err != nil {
		return err
	}

	pools, next, err := c.ListArtifactPools(cmd.Context(), limit, cursor)
	if err != nil {
		return classifyError(err)
	}

	if format == "json" {
		return printJSONIndent(cmd, pools)
	}
	printTable(cmd, pools, next)
	return nil
}

func printTable(cmd *cobra.Command, pools []cpclient.ArtifactPool, next string) {
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tREPLICATION\tMEMBERS\tCREATED")
	for _, ap := range pools {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			ap.Name, rfString(ap.ReplicationFactor), membersString(ap), ap.CreatedAt)
	}
	_ = tw.Flush()
	if next != "" {
		printf(cmd, "\nMore results - next page:\n  %s --cursor %s\n", cmd.CommandPath(), next)
	}
}
