// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"github.com/spf13/cobra"

	"github.com/otherix/otherix/internal/version"
)

const (
	componentName = "agent"

	// defaultConfigPath is the on-disk location where the bootstrap
	// subcommand writes — and where the serve subcommand reads — the
	// runtime config. Operators may override via --config; the dev
	// workflow leaves the default.
	defaultConfigPath = "/etc/otherix/agent.yaml"
)

// newRootCmd builds the otherix-agent command tree. The bare binary
// (no subcommand) defaults to `serve` so existing systemd units
// (ExecStart=/usr/local/bin/otherix-agent) keep working without
// modification.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "otherix-agent",
		Short:         "Otherix node agent",
		Version:       version.Current().Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	serve := newServeCommand()
	root.AddCommand(serve)
	root.AddCommand(newBootstrapCommand())

	// Default to `serve` when invoked without a subcommand.
	root.RunE = serve.RunE
	root.Flags().AddFlagSet(serve.Flags())

	return root
}
