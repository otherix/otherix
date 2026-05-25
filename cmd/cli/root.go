// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"github.com/spf13/cobra"

	clustercmd "github.com/otherix/otherix/cmd/cli/cluster"
	configcmd "github.com/otherix/otherix/cmd/cli/config"
	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	nodecmd "github.com/otherix/otherix/cmd/cli/node"
	poolcmd "github.com/otherix/otherix/cmd/cli/pool"
	templatecmd "github.com/otherix/otherix/cmd/cli/template"
	"github.com/otherix/otherix/cmd/cli/vm"
	"github.com/otherix/otherix/internal/version"
)

// rootDefaultEndpoint matches deploy/config/api.example.yaml — the
// dev workflow speaks plain HTTP to 0.0.0.0:8080. The flag default is
// an empty string (so cliconfig.Resolve can distinguish "operator did
// not set it" from "operator wanted localhost"); this constant only
// surfaces in help text.
const rootDefaultEndpoint = "http://localhost:8080"

// newRootCmd builds the otherix command tree. Persistent flags
// registered here are inherited by every subcommand:
//
//   - --endpoint / --token / --cluster / --config feed cliconfig.Resolve
//   - subcommands call cliauth.BuildClient(cmd) to get a *cpclient.Client
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "otherix",
		Short:         "Otherix operator CLI",
		Long:          "otherix is the operator-facing CLI for the Otherix control plane and its agents.",
		Version:       version.Current().Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().String(cliauth.FlagEndpoint, "",
		"CP base URL (default "+rootDefaultEndpoint+" if neither config nor env supply one)")
	root.PersistentFlags().String(cliauth.FlagToken, "",
		"API token (overrides $OTHERIX_API_TOKEN and stored cluster token)")
	root.PersistentFlags().String(cliauth.FlagCluster, "",
		"named cluster from the config file (overrides current-cluster)")
	root.PersistentFlags().String(cliauth.FlagConfig, "",
		"config file path (default $OTHERIX_CONFIG, then ~/.otherix/config)")

	root.AddCommand(vm.NewCommand())
	root.AddCommand(templatecmd.NewCommand())
	root.AddCommand(poolcmd.NewCommand())
	root.AddCommand(nodecmd.NewCommand())
	root.AddCommand(clustercmd.NewCommand())
	root.AddCommand(configcmd.NewCommand())

	return root
}
