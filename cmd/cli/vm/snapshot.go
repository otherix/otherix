// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

const (
	flagSnapshotName        = "name"
	flagSnapshotDescription = "description"
	flagSnapshotLimit       = "limit"
	flagSnapshotCursor      = "cursor"

	defaultSnapshotListLimit = 50
)

// newSnapshotCommand returns the `otherix vm snapshot` subcommand group and its
// children (create / list / get / delete). A snapshot is a content-addressed,
// disk-only point-in-time copy of a VM's disks. create / delete are async (the
// CP enqueues a task the agent drains); list / get are read paths. Mirrors the
// `otherix migration` group structure.
func newSnapshotCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Manage VM snapshots (disk-only, content-addressed).",
		Long: `snapshot groups the operator subcommands for VM snapshots - create a
snapshot of a VM, list a VM's snapshots, get a snapshot by id, and delete a
snapshot. A snapshot is a disk-only, crash-consistent, content-addressed copy
of the VM's disks (RAM is not captured). Recreate a VM from a snapshot with
'otherix vm create <name> --from-snapshot <id>'. Authentication and endpoint
resolution flow through the root-level --endpoint / --token / --cluster /
--config flags.`,
	}
	cmd.AddCommand(newSnapshotCreateCommand())
	cmd.AddCommand(newSnapshotListCommand())
	cmd.AddCommand(newSnapshotGetCommand())
	cmd.AddCommand(newSnapshotDeleteCommand())
	return cmd
}

func newSnapshotCreateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <vm>",
		Short: "Create a snapshot of a VM (async).",
		Long: `Sends POST /v1/vms/<vm>/snapshots to the Control Plane, which enqueues a
vm.snapshot.create task. The snapshot is disk-only and crash-consistent (RAM is
not captured). --name is the snapshot name, unique within the VM. --wait blocks
until the backing task reaches a terminal status; --wait-timeout bounds only how
long the CLI blocks (the snapshot keeps producing server-side regardless).`,
		Args: cobra.ExactArgs(1),
		RunE: runSnapshotCreate,
	}
	cmd.Flags().String(flagSnapshotName, "", "snapshot name, unique within the VM (required)")
	cmd.Flags().String(flagSnapshotDescription, "", "optional free-text description")
	cmd.Flags().Bool(flagWait, false, "block until the snapshot task reaches terminal status")
	cmd.Flags().Duration(flagWaitTimeout, defaultWaitTO, "max time to wait when --wait is set")
	return cmd
}

func runSnapshotCreate(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	name, err := requireStringFlag(cmd, flagSnapshotName)
	if err != nil {
		return err
	}
	description, err := cmd.Flags().GetString(flagSnapshotDescription)
	if err != nil {
		return err
	}
	wait, err := cmd.Flags().GetBool(flagWait)
	if err != nil {
		return err
	}
	timeout, err := cmd.Flags().GetDuration(flagWaitTimeout)
	if err != nil {
		return err
	}

	res, err := c.CreateSnapshot(cmd.Context(), args[0], cpclient.SnapshotCreateBody{
		Name:        name,
		Description: description,
	})
	if err != nil {
		return classifyError(err)
	}

	printf(cmd, "snapshot task=%s status=%s\n", res.TaskID, res.Status)
	if !wait {
		return nil
	}
	return waitSnapshotTask(cmd, c, res.TaskID, timeout)
}

func newSnapshotListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list <vm>",
		Short: "List a VM's snapshots.",
		Long: `Cursor-paginated list of a VM's snapshots, newest-first. The next page's
cursor is printed below the table and is opaque - re-pass with --cursor. Full
ids and the per-disk manifest remain available via --output json.`,
		Args: cobra.ExactArgs(1),
		RunE: runSnapshotList,
	}
	cmd.Flags().Int(flagSnapshotLimit, defaultSnapshotListLimit, "page size (1..200)")
	cmd.Flags().String(flagSnapshotCursor, "", "opaque cursor from a previous page")
	cmd.Flags().StringP(flagOutput, "o", "table", "output format: table|json|yaml")
	return cmd
}

func runSnapshotList(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	limit, err := cmd.Flags().GetInt(flagSnapshotLimit)
	if err != nil {
		return err
	}
	cursor, err := cmd.Flags().GetString(flagSnapshotCursor)
	if err != nil {
		return err
	}
	format, err := outputFormat(cmd, "table", "yaml")
	if err != nil {
		return err
	}

	snapshots, next, err := c.ListSnapshots(cmd.Context(), args[0], cpclient.ListSnapshotsParams{
		Limit:  limit,
		Cursor: cursor,
	})
	if err != nil {
		return classifyError(err)
	}

	switch format {
	case "json", "yaml":
		envelope := struct {
			Data []cpclient.Snapshot `json:"data"`
			Meta struct {
				NextCursor *string `json:"next_cursor"`
			} `json:"meta"`
		}{Data: snapshots}
		if next != "" {
			envelope.Meta.NextCursor = &next
		}
		raw, mErr := json.MarshalIndent(envelope, "", "  ")
		if mErr != nil {
			return fmt.Errorf("marshal json: %v", mErr)
		}
		printf(cmd, "%s\n", raw)
	default:
		printSnapshotTable(cmd, snapshots, next)
	}
	return nil
}

func newSnapshotGetCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <id>",
		Short: "Show a snapshot by id.",
		Long: `Fetches a snapshot's projected view from the CP by its UUID. The text view
surfaces the name, status, source VM state, architecture, and per-disk blob
digests. --output json echoes the server projection verbatim.`,
		Args: cobra.ExactArgs(1),
		RunE: runSnapshotGet,
	}
	cmd.Flags().StringP(flagOutput, "o", "text", "output format: text|json|yaml")
	return cmd
}

func runSnapshotGet(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	format, err := outputFormat(cmd, "text", "yaml")
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

func newSnapshotDeleteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a snapshot (async).",
		Long: `Sends DELETE /v1/snapshots/<id> to the Control Plane, which enqueues a
vm.snapshot.delete task that removes the snapshot's disk blobs. Delete is
fail-closed: a snapshot with non-deleted children is refused with 409. --wait
blocks until the teardown task reaches a terminal status.`,
		Args: cobra.ExactArgs(1),
		RunE: runSnapshotDelete,
	}
	cmd.Flags().Bool(flagWait, false, "block until the delete task reaches terminal status")
	cmd.Flags().Duration(flagWaitTimeout, defaultWaitTO, "max time to wait when --wait is set")
	return cmd
}

func runSnapshotDelete(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	wait, err := cmd.Flags().GetBool(flagWait)
	if err != nil {
		return err
	}
	timeout, err := cmd.Flags().GetDuration(flagWaitTimeout)
	if err != nil {
		return err
	}

	res, err := c.DeleteSnapshot(cmd.Context(), args[0])
	if err != nil {
		return classifyError(err)
	}

	// An empty task id is the synchronous-delete case (the snapshot never
	// reached the agent): nothing to poll.
	if res.TaskID == "" {
		printf(cmd, "snapshot %s deleted\n", args[0])
		return nil
	}

	printf(cmd, "snapshot delete task=%s status=%s\n", res.TaskID, res.Status)
	if !wait {
		return nil
	}
	return waitSnapshotTask(cmd, c, res.TaskID, timeout)
}

// waitSnapshotTask blocks on the snapshot task until terminal, mirroring
// runMigrate's --wait handling: a client-side wait-timeout is not a failure
// (the task keeps running server-side), so it prints a resume hint and exits 0.
func waitSnapshotTask(cmd *cobra.Command, c *cpclient.Client, rawTaskID string, timeout time.Duration) error {
	taskID, err := parseTaskID(rawTaskID)
	if err != nil {
		return fmt.Errorf("request_failed: %v", err)
	}
	if err := waitForTask(cmd.Context(), cmd, c, taskID, timeout); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			printf(cmd, "wait timed out after %s; snapshot task still running server-side\n", timeout)
			printf(cmd, "resume watching: poll task %s\n", rawTaskID)
			return nil
		}
		return classifyError(err)
	}
	printf(cmd, "snapshot complete task=%s\n", rawTaskID)
	return nil
}

// printSnapshotTable renders an aligned table to stdout. Columns:
// ID NAME STATUS VM-STATE SIZE AGE. The full UUID is shown (snapshot
// get / delete are UUID-keyed, no server-side prefix resolution); SIZE is
// the summed blob size ("-" until measured). The bottom hint prints
// next_cursor when populated so operators can chain the next page.
func printSnapshotTable(cmd *cobra.Command, snapshots []cpclient.Snapshot, next string) {
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tNAME\tSTATUS\tVM-STATE\tSIZE\tAGE")
	for _, s := range snapshots {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			s.ID, s.Name, s.Status, s.VMStateAtSnapshot,
			snapshotSize(s.DiskSizeBytes), humanAge(s.CreatedAt))
	}
	_ = tw.Flush()
	printNextCursor(cmd, next)
}

// printSnapshotText renders the operator-friendly multi-line key=value form.
// Nullable / unmeasured fields print "-". The per-disk manifest is shown one
// line per disk; full detail (raw bytes) is available via --output json.
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

// snapshotSize renders the summed blob size in IEC units, or "-" when the
// snapshot has not yet been measured (nil until status reaches ready).
func snapshotSize(b *int64) string {
	if b == nil {
		return "-"
	}
	return humanBytes(*b)
}

// humanBytes renders a byte count in IEC binary units (KiB / MiB / GiB / TiB)
// for the snapshot size columns; the JSON output keeps the raw bytes.
func humanBytes(n int64) string {
	const (
		kib int64 = 1024
		mib       = kib * 1024
		gib       = mib * 1024
		tib       = gib * 1024
	)
	switch {
	case n >= tib:
		return fmt.Sprintf("%.1fTiB", float64(n)/float64(tib))
	case n >= gib:
		return fmt.Sprintf("%.1fGiB", float64(n)/float64(gib))
	case n >= mib:
		return fmt.Sprintf("%.1fMiB", float64(n)/float64(mib))
	case n >= kib:
		return fmt.Sprintf("%.1fKiB", float64(n)/float64(kib))
	default:
		return fmt.Sprintf("%dB", n)
	}
}
