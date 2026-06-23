// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

const (
	flagMigrateNode          = "node"
	flagMigratePool          = "pool"
	flagMigrateOffline       = "offline"
	flagMigrateBandwidth     = "bandwidth"
	flagMigrateMaxDowntime   = "max-downtime"
	flagMigrateAllowPostcopy = "allow-postcopy"
)

// migrateFlags is the parsed flag set for `vm migrate`. Separated from cobra so
// buildMigrateBody is unit-testable without a live command.
type migrateFlags struct {
	node          string
	pool          string
	offline       bool
	bandwidth     string
	maxDowntimeMs int32
	allowPostcopy bool
}

// newMigrateCommand wires `otherix vm migrate <vm>` — live storage migration
// of the VM to another node (no --node lets the scheduler pick a
// different node). The CP enqueues a vm.migrate task whose resource is the
// migration; --wait blocks the CLI on the task, but the migration runs
// server-side regardless of the client (--wait-timeout is a client-side bound,
// not a server deadline). An explicit --node equal to the VM's current node is
// a no-op (already on target), reported as success, not an error.
func newMigrateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate <vm>",
		Short: "Live-migrate a VM to another node (async).",
		Long: `Sends POST /v1/vms/<vm>/migrate to the Control Plane, which enqueues a
vm.migrate task and creates a migration resource. Every Otherix migration is a
full storage migration (the VM disks cross the wire); --offline stops the VM,
copies, and restarts on the target instead of doing a live cutover.

--node <node>: target node (omitted: the scheduler picks a different eligible
node, per the node-less reschedule primitive). --pool <pool>: target storage
pool on that node (omitted: the cluster default pool). --bandwidth caps the
transfer rate (e.g. 100m, 1g, or raw bytes); --max-downtime sets the live
cutover stop-the-world budget in milliseconds; --allow-postcopy opts into
post-copy escalation.

--wait blocks until the backing task reaches a terminal status. --wait-timeout
bounds only how long the CLI blocks: on expiry the migration keeps running
server-side and the CLI prints how to resume watching. A migration that sits in
'pending' (no eligible target yet) is reported as such, not as an error.`,
		Args: cobra.ExactArgs(1),
		RunE: runMigrate,
	}
	cmd.Flags().String(flagMigrateNode, "", "target node name (omitted: scheduler picks)")
	cmd.Flags().String(flagMigratePool, "", "target storage pool name (omitted: cluster default)")
	cmd.Flags().Bool(flagMigrateOffline, false, "stop+copy+start instead of a live cutover")
	cmd.Flags().String(flagMigrateBandwidth, "", "transfer rate cap (e.g. 100m, 1g, or raw bytes/s)")
	cmd.Flags().Int32(flagMigrateMaxDowntime, 0, "live cutover downtime budget in milliseconds")
	cmd.Flags().Bool(flagMigrateAllowPostcopy, false, "allow post-copy escalation")
	cmd.Flags().Bool(flagWait, false, "block until the migration task reaches terminal status")
	cmd.Flags().Duration(flagWaitTimeout, defaultWaitTO, "max time to wait when --wait is set")
	return cmd
}

func runMigrate(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	f, err := readMigrateFlags(cmd)
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

	body, err := buildMigrateBody(f)
	if err != nil {
		return err
	}

	res, err := c.StartMigration(cmd.Context(), args[0], body)
	if err != nil {
		return classifyError(err)
	}

	// 200 already_on_target: the explicit target is the VM's current node.
	// This is a success no-op (the spec is explicit it is not an error).
	if res.NoOp {
		printf(cmd, "vm %s already on node %s; nothing to migrate\n", args[0], res.CurrentNodeID)
		return nil
	}

	printf(cmd, "migration task=%s status=%s\n", res.Task.TaskID, res.Task.Status)
	if !wait {
		return nil
	}

	taskID, err := parseTaskID(res.Task.TaskID)
	if err != nil {
		return fmt.Errorf("request_failed: %v", err)
	}
	if err := waitForTask(cmd.Context(), cmd, c, taskID, timeout); err != nil {
		// --wait-timeout is client-side only: it bounds how long the CLI
		// blocks, NOT the migration. On expiry the migration keeps running
		// server-side, so print the resume hint and exit 0 (fire-and-forget
		// is the point of async). A genuine task failure surfaces as an error.
		if errors.Is(err, context.DeadlineExceeded) {
			printf(cmd, "wait timed out after %s; migration still running server-side\n", timeout)
			printf(cmd, "resume watching: otherix vm get %s   (or poll task %s)\n", args[0], res.Task.TaskID)
			return nil
		}
		return classifyError(err)
	}
	printf(cmd, "vm migrate complete task=%s\n", res.Task.TaskID)
	return nil
}

// readMigrateFlags pulls the migrate-specific flags off the cobra command.
func readMigrateFlags(cmd *cobra.Command) (migrateFlags, error) {
	var f migrateFlags
	var err error
	if f.node, err = cmd.Flags().GetString(flagMigrateNode); err != nil {
		return f, err
	}
	if f.pool, err = cmd.Flags().GetString(flagMigratePool); err != nil {
		return f, err
	}
	if f.offline, err = cmd.Flags().GetBool(flagMigrateOffline); err != nil {
		return f, err
	}
	if f.bandwidth, err = cmd.Flags().GetString(flagMigrateBandwidth); err != nil {
		return f, err
	}
	if f.maxDowntimeMs, err = cmd.Flags().GetInt32(flagMigrateMaxDowntime); err != nil {
		return f, err
	}
	if f.allowPostcopy, err = cmd.Flags().GetBool(flagMigrateAllowPostcopy); err != nil {
		return f, err
	}
	return f, nil
}

// buildMigrateBody maps the parsed flags onto the wire body. live defaults to
// true; --offline flips it. Unset node/pool/knobs stay nil so the server
// applies its defaults (scheduler-placed target, cluster default pool).
func buildMigrateBody(f migrateFlags) (cpclient.MigrateBody, error) {
	body := cpclient.MigrateBody{
		Live:          !f.offline,
		AllowPostcopy: f.allowPostcopy,
	}
	if f.node != "" {
		node := f.node
		body.TargetNode = &node
	}
	if f.pool != "" {
		pool := f.pool
		body.TargetPool = &pool
	}
	bw, err := parseBandwidth(f.bandwidth)
	if err != nil {
		return cpclient.MigrateBody{}, err
	}
	body.MaxBandwidthBytes = bw
	if f.maxDowntimeMs > 0 {
		ms := f.maxDowntimeMs
		body.MaxDowntimeMs = &ms
	}
	return body, nil
}

// parseBandwidth converts an operator-friendly rate string into a byte count.
// A plain integer is bytes; k/m/g are SI (1000-based), ki/mi/gi are IEC
// (1024-based). An empty string yields nil so the server applies no cap.
// Negative or malformed input is a usage error.
func parseBandwidth(raw string) (*int64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, nil
	}
	lower := strings.ToLower(s)
	var mult int64 = 1
	switch {
	case strings.HasSuffix(lower, "ki"):
		mult, lower = 1024, strings.TrimSuffix(lower, "ki")
	case strings.HasSuffix(lower, "mi"):
		mult, lower = 1024*1024, strings.TrimSuffix(lower, "mi")
	case strings.HasSuffix(lower, "gi"):
		mult, lower = 1024*1024*1024, strings.TrimSuffix(lower, "gi")
	case strings.HasSuffix(lower, "k"):
		mult, lower = 1_000, strings.TrimSuffix(lower, "k")
	case strings.HasSuffix(lower, "m"):
		mult, lower = 1_000_000, strings.TrimSuffix(lower, "m")
	case strings.HasSuffix(lower, "g"):
		mult, lower = 1_000_000_000, strings.TrimSuffix(lower, "g")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(lower), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("--%s: invalid rate %q (use bytes, or a k/m/g/ki/mi/gi suffix)", flagMigrateBandwidth, raw)
	}
	if n < 0 {
		return nil, fmt.Errorf("--%s: rate must be non-negative", flagMigrateBandwidth)
	}
	v := n * mult
	return &v, nil
}
