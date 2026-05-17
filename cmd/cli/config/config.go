// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package config hosts the `otherix config` cobra subcommand tree.
// The subcommands manage а kubectl-style YAML credential store at
// $OTHERIX_CONFIG / ~/.otherix/config — see the cliconfig package
// for the storage layer и precedence semantics.
package config

import "github.com/spf13/cobra"

// NewCommand returns the root `otherix config` command, populated
// с the add / list / use / remove / show children. The tree is
// constructed lazily per call so tests can rebuild а fresh CLI
// без process-global cobra state.
func NewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage CLI cluster credentials (`otherix config add cluster`, list, use, remove, show)",
		Long: `config manages а small YAML credential store on disk so otherix
subcommands can pick the (endpoint, token) pair automatically
instead of taking --token on every invocation. The file lives at
$OTHERIX_CONFIG, falling back to ~/.otherix/config.

The model mirrors kubectl: each named cluster carries а server URL
plus an API token, и а current-cluster pointer picks which one
unspecified subcommands use. The --cluster flag overrides for one
invocation, и --token / --endpoint / $OTHERIX_API_TOKEN /
$OTHERIX_SERVER remain available as higher-precedence overrides.`,
	}

	cmd.AddCommand(newAddCommand())
	cmd.AddCommand(newListCommand())
	cmd.AddCommand(newUseCommand())
	cmd.AddCommand(newRemoveCommand())
	cmd.AddCommand(newShowCommand())

	return cmd
}
