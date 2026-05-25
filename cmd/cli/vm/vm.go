// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package vm hosts the `otherix vm` cobra subcommand group and its
// children (create / get / list / delete). Heavy lifting lives in
// cmd/cli/internal/cpclient; this package owns flag plumbing,
// output formatting, and operator-friendly error rendering.
package vm

import (
	"github.com/spf13/cobra"
)

// Local flag names — endpoint and token now live on the root cobra
// command (persistent), inherited automatically by everything in
// this group. The names listed here are the strictly-local flags
// vm subcommands own (--output, --wait, --wait-timeout).
const (
	flagOutput      = "output"
	flagWait        = "wait"
	flagWaitTimeout = "wait-timeout"
)

// NewCommand returns the `otherix vm` subcommand group, ready to be
// registered onto the root cobra tree by main.
func NewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vm",
		Short: "Manage virtual machines (CP /v1/vms surface)",
		Long: `vm groups the operator subcommands against the Control Plane's
/v1/vms surface — create, get, list, delete. Authentication and
endpoint resolution flow through the root-level --endpoint /
--token / --cluster / --config flags (see 'otherix --help' and
'otherix config' for the kubectl-style cluster store).`,
	}

	cmd.AddCommand(newCreateCommand())
	cmd.AddCommand(newGetCommand())
	cmd.AddCommand(newListCommand())
	cmd.AddCommand(newDeleteCommand())
	cmd.AddCommand(newPauseCommand())
	cmd.AddCommand(newResumeCommand())
	cmd.AddCommand(newResetCommand())
	cmd.AddCommand(newStartCommand())
	cmd.AddCommand(newStopCommand())
	cmd.AddCommand(newPoweroffCommand())
	cmd.AddCommand(newRebootCommand())
	cmd.AddCommand(newConsoleCommand())
	cmd.AddCommand(newLogsCommand())

	return cmd
}
