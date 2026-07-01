// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package lb hosts the `otherix lb` cobra subcommand group and its
// children - the CRUD surface (create / list / get / delete / update)
// over the Control Plane's /v1/loadbalancers endpoints.
//
// A load balancer is a named L4 front for the VMs whose labels match its
// selector; port is the guest TCP port ingress connections target. Unlike
// networks, the CP GET / PATCH / DELETE routes address a load balancer by
// NAME (the `{id}` path param is the name), so there is no client-side
// name->uuid resolution.
package lb

import "github.com/spf13/cobra"

// NewCommand returns the `otherix lb` subcommand group, ready to be
// registered onto the root cobra tree by main.
func NewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "lb",
		Aliases: []string{"loadbalancer"},
		Short:   "Manage load balancers (CP /v1/loadbalancers surface)",
		Long: `lb groups the operator-facing commands against the Control Plane's
/v1/loadbalancers surface. A load balancer is a named L4 front for the
VMs whose labels match its selector; a connection to the load balancer's
port is brokered to one matching backend VM.`,
	}
	cmd.AddCommand(newCreateCommand())
	cmd.AddCommand(newListCommand())
	cmd.AddCommand(newGetCommand())
	cmd.AddCommand(newDeleteCommand())
	cmd.AddCommand(newUpdateCommand())
	return cmd
}
