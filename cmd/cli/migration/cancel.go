// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package migration

import (
	"github.com/spf13/cobra"
)

func newCancelCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cancel <id>",
		Short: "Cancel a migration (best-effort).",
		Long: `Cancels a migration by its UUID. Cancel is sync and best-effort (valid
only pre-cutover): the VM is never moved off its source, so a cancelled
migration is fail-safe-to-source. The command is idempotent — cancelling an
already-terminal migration (completed / failed / cancelled) returns it
unchanged. Prints the resulting phase.`,
		Args: cobra.ExactArgs(1),
		RunE: runCancel,
	}
	return cmd
}

func runCancel(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}

	m, err := c.CancelMigration(cmd.Context(), args[0])
	if err != nil {
		return classifyError(err)
	}

	printf(cmd, "migration %s: %s\n", m.ID, m.Phase)
	return nil
}
