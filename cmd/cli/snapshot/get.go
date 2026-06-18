// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package snapshot

import (
	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

func newGetCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <id>",
		Short: "Show a snapshot by id.",
		Long: `Fetches a snapshot's projected view from the CP by its UUID. The text view
surfaces the name, status, source VM state, architecture, summed disk size, and
per-disk blob digests. --output json echoes the server projection verbatim.`,
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

	s, raw, err := c.Snapshot(cmd.Context(), args[0])
	if err != nil {
		return classifyError(err)
	}

	switch format {
	case "json", "yaml":
		return printJSON(cmd, raw)
	default:
		printSnapshotText(cmd, s)
		return nil
	}
}

// printSnapshotText renders the operator-friendly multi-line key=value form.
// Nullable / unmeasured fields print "-" (size) or are omitted (optional
// description / parent / error). The per-disk manifest is shown one line per
// disk; full detail (raw bytes) is available via --output json.
func printSnapshotText(cmd *cobra.Command, s cpclient.Snapshot) {
	printf(cmd, "id: %s\n", s.ID)
	printf(cmd, "vm_id: %s\n", s.VMID)
	printf(cmd, "name: %s\n", s.Name)
	printf(cmd, "status: %s\n", s.Status)
	printf(cmd, "vm_state_at_snapshot: %s\n", s.VMStateAtSnapshot)
	printf(cmd, "architecture: %s\n", s.Architecture)
	printf(cmd, "disk_size: %s\n", snapshotSize(s.DiskSizeBytes))
	if s.Description != "" {
		printf(cmd, "description: %s\n", s.Description)
	}
	if s.ParentSnapshotID != nil && *s.ParentSnapshotID != "" {
		printf(cmd, "parent_snapshot_id: %s\n", *s.ParentSnapshotID)
	}
	if len(s.Disks) > 0 {
		printf(cmd, "disks:\n")
		for _, d := range s.Disks {
			printf(cmd, "  %s sha256=%s size=%s\n", d.Device, d.SHA256, humanBytes(d.SizeBytes))
		}
	}
	if s.ErrorMessage != nil && *s.ErrorMessage != "" {
		printf(cmd, "error: %s\n", *s.ErrorMessage)
	}
	printf(cmd, "created_at: %s\n", s.CreatedAt)
	printf(cmd, "updated_at: %s\n", s.UpdatedAt)
}
