// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package lb

import (
	"strings"

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
	if lb.PublishedPort != nil {
		printf(cmd, "published_port: %d\n", *lb.PublishedPort)
		printf(cmd, "protocol: %s\n", lb.Protocol)
		if len(lb.SourceCIDRs) > 0 {
			printf(cmd, "source_cidrs: %s\n", strings.Join(lb.SourceCIDRs, ","))
		}
	}
	if h := lb.Health; h != nil {
		printf(cmd, "health: %s (%d/%d healthy)\n", h.Status, h.TargetsHealthy, h.TargetsTotal)
	}
	hc := lb.HealthCheck
	printf(cmd, "health_check:\n")
	printf(cmd, "  port: %d\n", derefInt(hc.Port))
	printf(cmd, "  interval_seconds: %d\n", derefInt(hc.IntervalSeconds))
	printf(cmd, "  timeout_seconds: %d\n", derefInt(hc.TimeoutSeconds))
	printf(cmd, "  healthy_threshold: %d\n", derefInt(hc.HealthyThreshold))
	printf(cmd, "  unhealthy_threshold: %d\n", derefInt(hc.UnhealthyThreshold))
	if len(lb.Backends) > 0 {
		printf(cmd, "backends:\n")
		for _, b := range lb.Backends {
			printf(cmd, "  - %s  healthy=%s  last_probed=%s\n", b.VMName, healthyLabel(b.Healthy), reportedLabel(b.ReportedAt))
		}
	}
	printf(cmd, "created_at: %s\n", lb.CreatedAt)
	printf(cmd, "updated_at: %s\n", lb.UpdatedAt)
	return nil
}

// derefInt returns the pointed-to int, or 0 when nil. A load-balancer view
// from the CP always fills every health-check field, so the nil path is only
// a defensive fallback.
func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// healthyLabel renders a backend's debounced health verdict: true / false, or
// "unknown" when no verdict has been reported yet (a warming backend).
func healthyLabel(h *bool) string {
	if h == nil {
		return "unknown"
	}
	if *h {
		return "true"
	}
	return "false"
}

// reportedLabel renders the last-probe timestamp, or "-" when none has been
// reported yet.
func reportedLabel(r *string) string {
	if r == nil || *r == "" {
		return "-"
	}
	return *r
}
