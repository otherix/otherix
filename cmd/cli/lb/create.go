// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package lb

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// newCreateCommand returns the `otherix lb create` cobra command. One
// invocation registers one load balancer.
func newCreateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a load balancer.",
		Long: `Submits POST /v1/loadbalancers. A load balancer is a named L4 front
for the VMs whose labels match --selector; --port is the guest TCP port
ingress connections target.

--selector is a comma-separated k=v list; a VM is an eligible backend
when its labels match every entry.

Example:
  otherix lb create web --port 8080 --selector app=web,tier=fe`,
		Args: cobra.ExactArgs(1),
		RunE: runCreate,
	}
	cmd.Flags().Int(flagPort, 0, "guest TCP port ingress connections target (1..65535, required)")
	cmd.Flags().String(flagSelector, "", "backend selector as k=v[,k=v...] (required)")
	cmd.Flags().Bool(flagPublish, false, "give the load balancer a public listener on gateway nodes")
	cmd.Flags().Int(flagPublishedPort, 0, "public TCP port (default: --port); implies --publish")
	cmd.Flags().StringArray(flagSourceCIDR, nil, "restrict the public listener to a client CIDR (repeatable); implies --publish")
	registerHealthCheckFlags(cmd)
	cmd.Flags().StringP(flagOutput, "o", "text", "output format: text|json")
	cmd.Flags().Bool(flagShowIDs, false, "include the load balancer UUID in the text output")
	_ = cmd.MarkFlagRequired(flagPort)
	_ = cmd.MarkFlagRequired(flagSelector)
	return cmd
}

func runCreate(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	name := args[0]
	if name == "" {
		return errors.New("load balancer name is required")
	}

	port, err := cmd.Flags().GetInt(flagPort)
	if err != nil {
		return err
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("invalid --port %d: must be in 1..65535", port)
	}
	selectorRaw, err := cmd.Flags().GetString(flagSelector)
	if err != nil {
		return err
	}
	selector, err := parseSelector(selectorRaw)
	if err != nil {
		return err
	}
	format, err := outputFormat(cmd, "text")
	if err != nil {
		return err
	}
	showIDs, _ := cmd.Flags().GetBool(flagShowIDs)

	healthCheck, err := healthCheckFromFlags(cmd)
	if err != nil {
		return err
	}

	params := cpclient.CreateLoadBalancerParams{
		Name:        name,
		Port:        int32(port), //nolint:gosec // port validated in 1..65535 above.
		Selector:    selector,
		HealthCheck: healthCheck,
	}
	if err := applyPublishFlags(cmd, &params, port); err != nil {
		return err
	}

	created, err := c.CreateLoadBalancer(cmd.Context(), params)
	if err != nil {
		return classifyError(err)
	}

	return renderCreateOutput(cmd, created, format, showIDs)
}

// applyPublishFlags folds the --publish / --publish-port / --source-cidr flags
// into params. Either --publish-port or --source-cidr implies --publish; a bare
// --publish with no --publish-port defaults the published port to the backend
// port. When publishing, Protocol is fixed to tcp (the only supported L4
// protocol). backendPort is the already-validated --port value.
func applyPublishFlags(cmd *cobra.Command, params *cpclient.CreateLoadBalancerParams, backendPort int) error {
	publish, _ := cmd.Flags().GetBool(flagPublish)
	pubPort, _ := cmd.Flags().GetInt(flagPublishedPort)
	sourceCIDRs, _ := cmd.Flags().GetStringArray(flagSourceCIDR)
	if pubPort != 0 || len(sourceCIDRs) > 0 {
		publish = true
	}
	if !publish {
		return nil
	}
	if pubPort == 0 {
		pubPort = backendPort
	}
	if pubPort < 1 || pubPort > 65535 {
		return fmt.Errorf("invalid --%s %d: must be in 1..65535", flagPublishedPort, pubPort)
	}
	pp := int32(pubPort) //nolint:gosec // pubPort validated in 1..65535 above.
	params.PublishedPort = &pp
	params.Protocol = "tcp"
	params.SourceCIDRs = sourceCIDRs
	return nil
}

// renderCreateOutput writes the JSON or text representation of the
// created load balancer.
func renderCreateOutput(cmd *cobra.Command, lb cpclient.LoadBalancer, format string, showIDs bool) error {
	if format == "json" {
		raw, err := json.MarshalIndent(lb, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
		return nil
	}
	printf(cmd, "load balancer %s created\n", lb.Name)
	if showIDs {
		printf(cmd, "id: %s\n", lb.ID)
	}
	printf(cmd, "port: %d\n", lb.Port)
	printf(cmd, "selector: %s\n", formatSelector(lb.Selector))
	return nil
}
