// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"github.com/spf13/cobra"
)

// defaultConfigPath is where serve reads, and join writes, the api-server
// config (Otherix convention: /etc/otherix for operator config).
const defaultConfigPath = "/etc/otherix/api.yaml"

// newRootCmd builds the otherix-api command tree. The bare binary
// (no subcommand) defaults to `serve` so the systemd unit
// (ExecStart=/usr/bin/otherix-api) keeps working unmodified.
//
// The root deliberately does NOT set cobra's Version field: doing so would
// register a built-in --version that prints the short "otherix-api version
// X" form and short-circuit RunE. Instead the serve flagset (added below)
// carries the --version bool, so the bare invocation reaches runServeCmd
// and prints the rich "otherix-api X (commit Y, built Z)" line - identical
// to the pre-cobra behavior operators and docs rely on.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "otherix-api",
		Short:         "Otherix control-plane API server (embedded etcd)",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	serve := newServeCommand()
	root.AddCommand(serve)
	root.AddCommand(newJoinCommand())

	root.RunE = serve.RunE
	root.Flags().AddFlagSet(serve.Flags())

	return root
}

// newJoinCommand is a TEMPORARY stub replaced by the real join
// subcommand in the next task. It exists so the command tree compiles.
func newJoinCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "join",
		Short: "Join this control plane to an existing HA cluster (stub).",
		RunE:  func(*cobra.Command, []string) error { return nil },
	}
}
