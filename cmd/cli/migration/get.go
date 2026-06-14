// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package migration

import (
	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

func newGetCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <id>",
		Short: "Show a migration's projection.",
		Long: `Fetches a migration's projected view from the CP. The id may be a full
UUID or a unique short hex prefix (>= 8 chars), which the CP resolves. The
text view surfaces the phase, transfer progress, source/target node names
and the reason. A migration in 'pending' is not an error - it is alive and
waiting for an eligible target; the scheduling reason is shown as a
'waiting:' line.`,
		Args: cobra.ExactArgs(1),
		RunE: runGet,
	}
	cmd.Flags().StringP(flagOutput, "o", "text", "output format: text|json|yaml")
	return cmd
}

func runGet(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	format, err := outputFormat(cmd, "text")
	if err != nil {
		return err
	}

	m, raw, err := c.Migration(cmd.Context(), args[0])
	if err != nil {
		return classifyError(err)
	}

	switch format {
	case "json", "yaml":
		// The migration view is flat JSON; yaml and json both echo the server
		// projection verbatim (no manifest projection exists for migrations,
		// they are not apply-able resources).
		return printJSON(cmd, raw)
	default:
		printMigrationText(cmd, m)
		return nil
	}
}

// printMigrationText renders the operator-friendly multi-line key=value form.
// Nullable fields print "<unset>". When the migration is still pending and the
// CP recorded a scheduling reason, a dedicated `waiting:` line surfaces why it
// has not been bound to a target yet — the spec is explicit that pending is not
// an error, so it is framed as a wait, not a failure.
func printMigrationText(cmd *cobra.Command, m cpclient.Migration) {
	printf(cmd, "id: %s\n", shortID(m.ID))
	printf(cmd, "vm: %s\n", vmLabel(m))
	printf(cmd, "status: %s\n", m.Phase)
	printf(cmd, "progress: %d%%\n", m.ProgressPercent)
	printf(cmd, "source_node: %s\n", nodeLabel(m.SourceNodeName, m.SourceNodeID))
	printf(cmd, "target_node: %s\n", nodeLabel(m.TargetNodeName, m.TargetNodeID))
	printf(cmd, "target_pool: %s\n", dashIfEmpty(m.TargetPoolName))
	printf(cmd, "reason: %s\n", m.Reason)
	printf(cmd, "live: %s\n", boolYesNo(m.Live))
	if m.Phase == "pending" && m.SchedulingReason != nil && *m.SchedulingReason != "" {
		printf(cmd, "waiting: %s\n", *m.SchedulingReason)
	}
	if m.ErrorMessage != nil && *m.ErrorMessage != "" {
		printf(cmd, "error: %s\n", *m.ErrorMessage)
	}
	printf(cmd, "created_at: %s\n", m.CreatedAt)
	if m.StartedAt != nil {
		printf(cmd, "started_at: %s\n", *m.StartedAt)
	}
	if m.CompletedAt != nil {
		printf(cmd, "completed_at: %s\n", *m.CompletedAt)
	}
}

func boolYesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
