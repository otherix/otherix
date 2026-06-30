// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"github.com/spf13/cobra"

	apitokencmd "github.com/otherix/otherix/cmd/cli/apitoken"
	artifactpoolcmd "github.com/otherix/otherix/cmd/cli/artifactpool"
	clustercmd "github.com/otherix/otherix/cmd/cli/cluster"
	configcmd "github.com/otherix/otherix/cmd/cli/config"
	forwardcmd "github.com/otherix/otherix/cmd/cli/forward"
	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	migrationcmd "github.com/otherix/otherix/cmd/cli/migration"
	networkcmd "github.com/otherix/otherix/cmd/cli/network"
	nodecmd "github.com/otherix/otherix/cmd/cli/node"
	poolcmd "github.com/otherix/otherix/cmd/cli/pool"
	snapshotcmd "github.com/otherix/otherix/cmd/cli/snapshot"
	sshcmd "github.com/otherix/otherix/cmd/cli/ssh"
	sshgrantcmd "github.com/otherix/otherix/cmd/cli/sshgrant"
	usercmd "github.com/otherix/otherix/cmd/cli/user"
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
	root.AddCommand(sshcmd.NewCommand())
	root.AddCommand(forwardcmd.NewCommand())
	root.AddCommand(sshgrantcmd.NewCommand())
	root.AddCommand(migrationcmd.NewCommand())
	root.AddCommand(snapshotcmd.NewCommand())
	root.AddCommand(poolcmd.NewCommand())
	root.AddCommand(artifactpoolcmd.NewCommand())
	root.AddCommand(networkcmd.NewCommand())
	root.AddCommand(nodecmd.NewCommand())
	root.AddCommand(usercmd.NewCommand())
	root.AddCommand(apitokencmd.NewCommand())
	root.AddCommand(clustercmd.NewCommand())
	root.AddCommand(configcmd.NewCommand())
	root.AddCommand(newManifestCreateCmd())
	root.AddCommand(newManifestDeleteCmd())

	return root
}
