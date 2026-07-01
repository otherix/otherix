// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package ingressgrant

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// newListCommand returns `otherix ingress-grant list`.
func newListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List ingress grants.",
		Long: `Cursor-paginated list of ingress grants. A developer sees only the grants
they created; admin and operator see all. The stored token is never surfaced.`,
		RunE: runList,
	}
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
	limit, _ := cmd.Flags().GetInt(flagLimit)
	cursor, _ := cmd.Flags().GetString(flagCursor)
	format, err := outputFormat(cmd, "table", "table", "yaml")
	if err != nil {
		return err
	}

	grants, err := c.ListIngressGrants(cmd.Context(), cpclient.ListIngressGrantsParams{
		Limit:  limit,
		Cursor: cursor,
	})
	if err != nil {
		return classifyError(err)
	}

	switch format {
	case "json":
		raw, err := json.MarshalIndent(grants, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
	case "yaml":
		out, err := yaml.Marshal(grants.Data)
		if err != nil {
			return fmt.Errorf("marshal yaml: %v", err)
		}
		printf(cmd, "%s", out)
	default:
		printGrantTable(cmd, grants)
	}
	return nil
}

// printGrantTable renders the grant list as an aligned table.
func printGrantTable(cmd *cobra.Command, grants cpclient.IngressGrantList) {
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tRECIPIENT\tVMS\tSTATUS\tEXPIRES")
	for _, g := range grants.Data {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			g.Name, dash(g.RecipientLabel), vmSummary(g.VMs), grantStatus(g), derefOr(g.ExpiresAt, "never"))
	}
	_ = tw.Flush()
	if grants.Meta.NextCursor != nil {
		printNextCursor(cmd, *grants.Meta.NextCursor)
	}
}

// vmSummary renders the grant's VM scope as a compact "vm:login,..." string.
func vmSummary(vms []cpclient.IngressGrantVM) string {
	if len(vms) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(vms))
	for _, vm := range vms {
		parts = append(parts, vm.VMName+":"+vm.Login)
	}
	return strings.Join(parts, ",")
}

// dash renders an empty string as "-" for table cells.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// printNextCursor prints a copy-pasteable next-page hint for a cursor-paginated
// listing. No-op when there is no next page.
func printNextCursor(cmd *cobra.Command, next string) {
	if next == "" {
		return
	}
	printf(cmd, "\nMore results - next page:\n  %s --cursor %s\n", cmd.CommandPath(), next)
}
