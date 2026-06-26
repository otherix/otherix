// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

const (
	flagDrainTimeout = "timeout"
	flagWait         = "wait"
	flagWaitTimeout  = "wait-timeout"

	// defaultWaitTO is the client-side --wait budget. Matches the value the
	// other async CLI verbs use (vm create / migrate / snapshot).
	defaultWaitTO = 10 * time.Minute
)

// drainResult mirrors the node.drain task result the CP writes
// (nodes.DrainResult). Migrated counts VMs that left the node; Remaining is
// what was still on it at finalize; InFlight is the subset of Remaining still
// migrating (they finish on their own); Stuck names the VMs with no eligible
// target. Code is set on a non-success outcome.
type drainResult struct {
	Code      string   `json:"code"`
	Migrated  int      `json:"migrated"`
	Remaining int      `json:"remaining"`
	InFlight  int      `json:"in_flight"`
	Stuck     []string `json:"stuck"`
}

// newDrainCommand wires `otherix node drain <node>` - evacuate a node's VMs to
// other eligible nodes, then leave it cordoned. The CP enqueues a node.drain
// saga (async, 202) that retries until the node is empty or the drain budget
// expires. --timeout overrides the cluster-default budget for this drain;
// --wait blocks the CLI on the task and renders the outcome; --wait-timeout
// bounds only how long the CLI blocks (the drain keeps running server-side).
func newDrainCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "drain <node>",
		Short: "Evacuate a node's VMs and leave it cordoned (async).",
		Long: `Sends POST /v1/nodes/<node>/drain to the Control Plane, which cordons
the node and enqueues a node.drain saga. The saga live-migrates each VM to an
eligible node; a VM with no eligible target is left running and reported as
stuck. When the drain finishes, the node stays cordoned.

--timeout <dur> overrides the cluster-default drain budget for this drain
(0: use the cluster default). On budget expiry the node is left cordoned with
its remaining VMs still on it.

--wait blocks until the drain task reaches a terminal status and renders the
outcome (migrated / remaining / in-flight, plus any stuck VMs). --wait-timeout
bounds only how long the CLI blocks: on expiry the drain keeps running
server-side and the CLI prints how to resume watching.`,
		Args: cobra.ExactArgs(1),
		RunE: runDrain,
	}
	cmd.Flags().Duration(flagDrainTimeout, 0, "drain budget for this drain (0: cluster default)")
	cmd.Flags().Bool(flagWait, false, "block until the drain task reaches terminal status")
	cmd.Flags().Duration(flagWaitTimeout, defaultWaitTO, "max time to wait when --wait is set")
	return cmd
}

func runDrain(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	node := args[0]

	timeout, err := cmd.Flags().GetDuration(flagDrainTimeout)
	if err != nil {
		return err
	}
	wait, err := cmd.Flags().GetBool(flagWait)
	if err != nil {
		return err
	}
	waitTimeout, err := cmd.Flags().GetDuration(flagWaitTimeout)
	if err != nil {
		return err
	}

	task, err := c.DrainNode(cmd.Context(), node, int32(timeout.Seconds()))
	if err != nil {
		return classifyError(err)
	}

	printf(cmd, "drain node=%s task=%s status=%s\n", node, task.TaskID, task.Status)
	if !wait {
		return nil
	}

	taskID, err := uuid.Parse(task.TaskID)
	if err != nil {
		return fmt.Errorf("malformed task id %q: %v", task.TaskID, err)
	}
	terminal, err := c.WaitTask(cmd.Context(), taskID, cpclient.WaitOptions{Timeout: waitTimeout})
	if err != nil {
		// --wait-timeout is client-side only: it bounds how long the CLI blocks,
		// NOT the drain. On expiry the drain keeps running server-side, so print
		// the resume hint and exit 0. A genuine poll failure surfaces as an error.
		if errors.Is(err, context.DeadlineExceeded) {
			printf(cmd, "wait timed out after %s; drain still running server-side\n", waitTimeout)
			printf(cmd, "resume watching: otherix node get %s   (or poll task %s)\n", node, task.TaskID)
			return nil
		}
		return classifyError(err)
	}
	return renderDrainResult(cmd, node, terminal)
}

// renderDrainResult prints the outcome of a terminal node.drain task: the
// migrated / remaining / in-flight counts, and (when VMs were left behind) the
// stuck-VM list plus the operator hint. A drain that times out with VMs still
// on the node is a normal operational outcome, not a CLI error - it is
// rendered, not raised.
func renderDrainResult(cmd *cobra.Command, node string, task cpclient.Task) error {
	var res drainResult
	if len(task.Result) > 0 && string(task.Result) != "null" {
		if err := json.Unmarshal(task.Result, &res); err != nil {
			return fmt.Errorf("decode drain result: %v", err)
		}
	}

	printf(cmd, "drain %s: %s\n", node, task.Status)
	printf(cmd, "  migrated=%d remaining=%d in_flight=%d\n", res.Migrated, res.Remaining, res.InFlight)
	if res.Code != "" {
		printf(cmd, "  reason: %s\n", res.Code)
	}
	if res.Remaining > 0 {
		if len(res.Stuck) > 0 {
			printf(cmd, "  stuck VMs (no eligible target):\n")
			for _, name := range res.Stuck {
				printf(cmd, "    - %s\n", name)
			}
		}
		printf(cmd, "  node left cordoned; add capacity and re-run drain, or migrate/stop the remaining VMs manually\n")
	}
	return nil
}
