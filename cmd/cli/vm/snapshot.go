// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

const (
	flagSnapshotName         = "name"
	flagSnapshotDescription  = "description"
	flagSnapshotArtifactPool = "artifact-pool"
)

// newSnapshotCommand wires `otherix vm snapshot <vm>` - take a disk-only,
// crash-consistent, content-addressed snapshot of a VM's disks (RAM is not
// captured). The VM is the positional arg; --name is the snapshot name and is
// OPTIONAL: when omitted it defaults to `snap<unix_seconds>` and the chosen
// name is printed so the operator sees it. The CP enqueues a
// vm.snapshot.create task the agent drains; --wait blocks the CLI on the task
// (--wait-timeout is a client-side bound, not a server deadline). Mirrors the
// shape of `otherix vm migrate`; the standalone snapshot resource is queried
// under the top-level `otherix snapshot` group.
func newSnapshotCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "snapshot <vm>",
		Short: "Snapshot a VM's disks (async).",
		Long: `Sends POST /v1/vms/<vm>/snapshots to the Control Plane, which enqueues a
vm.snapshot.create task. The snapshot is disk-only and crash-consistent (RAM is
not captured). --name is the snapshot name, unique within the VM; when omitted it
defaults to 'snap<unix_seconds>' and the chosen name is printed. --wait blocks
until the backing task reaches a terminal status; --wait-timeout bounds only how
long the CLI blocks (the snapshot keeps producing server-side regardless).

List / get / delete snapshots through the top-level 'otherix snapshot' group.`,
		Args: cobra.ExactArgs(1),
		RunE: runSnapshot,
	}
	cmd.Flags().String(flagSnapshotName, "", "snapshot name, unique within the VM (default: snap<unix_seconds>)")
	cmd.Flags().String(flagSnapshotDescription, "", "optional free-text description")
	cmd.Flags().String(flagSnapshotArtifactPool, "", "artifact pool for the snapshot (default: cluster default artifact pool)")
	cmd.Flags().Bool(flagWait, false, "block until the snapshot task reaches terminal status")
	cmd.Flags().Duration(flagWaitTimeout, defaultWaitTO, "max time to wait when --wait is set")
	return cmd
}

func runSnapshot(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	name, err := cmd.Flags().GetString(flagSnapshotName)
	if err != nil {
		return err
	}
	description, err := cmd.Flags().GetString(flagSnapshotDescription)
	if err != nil {
		return err
	}
	artifactPool, err := cmd.Flags().GetString(flagSnapshotArtifactPool)
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

	// --name is optional: default to snap<unix_seconds> and surface the chosen
	// name so the operator knows what to query later.
	if name == "" {
		name = "snap" + strconv.FormatInt(time.Now().Unix(), 10)
		printf(cmd, "snapshot name: %s\n", name)
	}

	res, err := c.CreateSnapshot(cmd.Context(), args[0], cpclient.SnapshotCreateBody{
		Name:         name,
		Description:  description,
		ArtifactPool: artifactPool,
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
