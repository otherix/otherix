// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package snapshot hosts the `otherix snapshot` cobra subcommand group
// (list / get / delete) against the Control Plane's /v1/snapshots surface. A
// snapshot is a content-addressed, disk-only point-in-time copy of a VM's
// disks (RAM is not captured). A snapshot is *created* through
// `otherix vm snapshot <vm>` (it is a VM sub-resource action), never here; this
// group lists snapshots (globally, filtered by VM / status), gets a snapshot by
// id, and deletes a snapshot. Heavy lifting lives in
// cmd/cli/internal/cpclient; this package owns flag plumbing and output
// formatting. Mirrors the `otherix migration` group structure.
package snapshot

import (
	"github.com/spf13/cobra"
)

// NewCommand returns the `otherix snapshot` subcommand group, ready to be
// registered onto the root cobra tree by main.
func NewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Inspect VM snapshots (CP /v1/snapshots surface)",
		Long: `snapshot groups the operator subcommands against the Control
Plane's /v1/snapshots surface - list snapshots (globally, filtered by VM or
status), get a snapshot by id, and delete a snapshot. A snapshot is a disk-only,
crash-consistent, content-addressed copy of a VM's disks (RAM is not captured).
Snapshots are *created* through 'otherix vm snapshot <vm>' (a VM sub-resource
action), never here. Authentication and endpoint resolution flow through the
root-level --endpoint / --token / --cluster / --config flags.`,
	}

	cmd.AddCommand(newListCommand())
	cmd.AddCommand(newGetCommand())
	cmd.AddCommand(newDeleteCommand())

	return cmd
}
