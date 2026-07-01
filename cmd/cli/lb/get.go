// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package lb

import (
	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
	"github.com/otherix/otherix/cmd/cli/internal/manifest"
)

func newGetCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Show a load balancer.",
		Long: `Fetches a single load balancer by name. The CP GET-by-id route
addresses the load balancer by name (the path param is the name, not a
UUID), so the positional is passed through verbatim.`,
		Args: cobra.ExactArgs(1),
		RunE: runGet,
	}
	cmd.Flags().StringP(flagOutput, "o", "text", "output format: text|json|yaml")
	return cmd
}

func runGet(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	name := args[0]
	format, err := outputFormat(cmd, "text", "yaml")
	if err != nil {
		return err
	}

	lb, raw, err := c.GetLoadBalancer(cmd.Context(), name)
	if err != nil {
		return classifyError(err)
	}
	switch format {
	case "json":
		return printJSON(cmd, raw)
	case "yaml":
		out, perr := manifest.ProjectLoadBalancer(lb)
		if perr != nil {
			return perr
		}
		printf(cmd, "%s", out)
		return nil
	}
	return renderGet(cmd, lb)
}

func renderGet(cmd *cobra.Command, lb cpclient.LoadBalancer) error {
	printf(cmd, "id: %s\n", lb.ID)
	printf(cmd, "name: %s\n", lb.Name)
	printf(cmd, "owner_id: %s\n", lb.OwnerID)
	printf(cmd, "port: %d\n", lb.Port)
	printf(cmd, "selector: %s\n", formatSelector(lb.Selector))
	printf(cmd, "created_at: %s\n", lb.CreatedAt)
	printf(cmd, "updated_at: %s\n", lb.UpdatedAt)
	return nil
}
