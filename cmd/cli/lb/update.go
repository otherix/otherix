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

func newUpdateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update <name>",
		Short: "Update a load balancer's port and/or selector.",
		Long: `Submits PATCH /v1/loadbalancers/{name}. The positional is the load
balancer name (the CP route addresses by name). At least one of --port
or --selector must be supplied; an omitted flag is left untouched.

--selector replaces the whole selector with a fresh k=v[,k=v...] set.

Example:
  otherix lb update web --port 9090
  otherix lb update web --selector app=web,tier=fe`,
		Args: cobra.ExactArgs(1),
		RunE: runUpdate,
	}
	cmd.Flags().Int(flagPort, 0, "guest TCP port ingress connections target (1..65535)")
	cmd.Flags().String(flagSelector, "", "backend selector as k=v[,k=v...]")
	cmd.Flags().String(flagOutput, "text", "output format: text|json")
	return cmd
}

func runUpdate(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	name := args[0]
	if name == "" {
		return errors.New("load balancer name is required")
	}
	format, err := outputFormat(cmd, "text")
	if err != nil {
		return err
	}

	portChanged := cmd.Flags().Changed(flagPort)
	selectorChanged := cmd.Flags().Changed(flagSelector)
	if !portChanged && !selectorChanged {
		return errors.New("specify --port and/or --selector")
	}

	var params cpclient.UpdateLoadBalancerParams
	if portChanged {
		port, err := cmd.Flags().GetInt(flagPort)
		if err != nil {
			return err
		}
		p := int32(port)
		params.Port = &p
	}
	if selectorChanged {
		selectorRaw, err := cmd.Flags().GetString(flagSelector)
		if err != nil {
			return err
		}
		selector, err := parseSelector(selectorRaw)
		if err != nil {
			return err
		}
		params.Selector = selector
	}

	updated, err := c.UpdateLoadBalancer(cmd.Context(), name, params)
	if err != nil {
		return classifyError(err)
	}

	if format == "json" {
		raw, err := json.MarshalIndent(updated, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
		return nil
	}
	return renderGet(cmd, updated)
}
