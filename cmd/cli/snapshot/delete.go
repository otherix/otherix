// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package snapshot

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

func newDeleteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a snapshot (async).",
		Long: `Sends DELETE /v1/snapshots/<id> to the Control Plane, which enqueues a
vm.snapshot.delete task that removes the snapshot's disk blobs. Delete is
fail-closed: a snapshot with non-deleted children is refused with 409. --wait
blocks until the teardown task reaches a terminal status; --wait-timeout bounds
only how long the CLI blocks (the teardown keeps running server-side regardless).`,
		Args: cobra.ExactArgs(1),
		RunE: runDelete,
	}
	cmd.Flags().Bool(flagWait, false, "block until the delete task reaches terminal status")
	cmd.Flags().Duration(flagWaitTimeout, defaultWaitTO, "max time to wait when --wait is set")
	return cmd
}

func runDelete(cmd *cobra.Command, args []string) error {
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

// waitSnapshotTask blocks on the snapshot task until terminal, mirroring the
// migrate / vm-delete --wait handling: a client-side wait-timeout is not a
// failure (the task keeps running server-side), so it prints a resume hint and
// exits 0.
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
