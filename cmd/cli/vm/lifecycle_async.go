// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// newStartCommand wires `otherix vm start <vm>` — async per CP spec.
// The CP enqueues a vm.start task; the agent boots the QEMU process
// and verifies QMP. --wait blocks until the task reaches terminal.
func newStartCommand() *cobra.Command {
	return newAsyncLifecycleCommand(
		"start",
		"Start a stopped VM (async).",
		`Sends POST /v1/vms/<vm>/start to the Control Plane. The CP enqueues a
vm.start task pinned to the VM's node; the agent boots the QEMU
process and verifies QMP responsiveness before reporting success.
Sets desired_phase = 'running' on success. Idempotent: starting an
already-running VM completes successfully without re-spawning.`,
		func(c *cpclient.Client, ctx context.Context, name string) (cpclient.TaskAccepted, error) {
			return c.StartVM(ctx, name)
		},
		false, // no --force flag
	)
}

// newStopCommand wires `otherix vm stop <vm>` — async graceful ACPI
// shutdown. --force dispatches to the poweroff endpoint
// (CLI-level dispatch, NOT a stop-with-force flag on the API).
func newStopCommand() *cobra.Command {
	return newAsyncLifecycleCommand(
		"stop",
		"Graceful ACPI shutdown of a running VM (async).",
		`Sends POST /v1/vms/<vm>/stop to the Control Plane. The CP enqueues a
vm.stop task; the agent issues QMP system_powerdown and waits up to
the server-configured shutdown grace for the guest to honour the
signal. On timeout the task completes as failed with code
stop_timeout — operators dispatch to the poweroff path to force.

--force: short-circuits to POST /v1/vms/<vm>/poweroff instead (hard
shutdown, guest not notified). This is a CLI-level dispatch to a
different endpoint, not a server-side flag.`,
		func(c *cpclient.Client, ctx context.Context, name string) (cpclient.TaskAccepted, error) {
			return c.StopVM(ctx, name)
		},
		true, // --force flag dispatches to poweroff
	)
}

// newPoweroffCommand wires `otherix vm poweroff <vm>` — async hard
// shutdown. Equivalent to pulling the power cable.
func newPoweroffCommand() *cobra.Command {
	return newAsyncLifecycleCommand(
		"poweroff",
		"Hard power-off of a VM (async).",
		`Sends POST /v1/vms/<vm>/poweroff to the Control Plane. The CP enqueues
a vm.poweroff task; the agent attempts QMP quit (clean socket
release) and falls back to SIGKILL after a short grace. The guest OS
is not notified — data loss inside the guest is possible. Sets
desired_phase = 'stopped' on success.`,
		func(c *cpclient.Client, ctx context.Context, name string) (cpclient.TaskAccepted, error) {
			return c.PoweroffVM(ctx, name)
		},
		false,
	)
}

// newRebootCommand wires `otherix vm reboot <vm>` — async stop+start
// cycle.
func newRebootCommand() *cobra.Command {
	return newAsyncLifecycleCommand(
		"reboot",
		"Graceful ACPI reboot of a running VM (async).",
		`Sends POST /v1/vms/<vm>/reboot to the Control Plane. The CP enqueues
a vm.reboot task; the agent orchestrates an internal stop+start —
the QEMU process is replaced so the PID changes (distinct from
reset, which preserves runtime identity through QMP system_reset).
On stop-phase timeout the task fails with stop_timeout; operators
dispatch to reset to force.`,
		func(c *cpclient.Client, ctx context.Context, name string) (cpclient.TaskAccepted, error) {
			return c.RebootVM(ctx, name)
		},
		false,
	)
}

// newAsyncLifecycleCommand is the shared builder for the four async
// lifecycle subcommands. They differ only in the cpclient method
// they invoke and (for stop) the --force flag that dispatches to the
// poweroff endpoint. All four support --wait / --wait-timeout.
//
// When forceFlag is true, --force is added to the command's flag set
// and dispatched to cpclient.PoweroffVM on parse time, which
// requires CLI-level dispatch rather than a server-
// side flag.
func newAsyncLifecycleCommand(
	use, short, long string,
	dispatch func(*cpclient.Client, context.Context, string) (cpclient.TaskAccepted, error),
	forceFlag bool,
) *cobra.Command {
	cmd := &cobra.Command{
		Use:   use + " <vm>",
		Short: short,
		Long:  long,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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

			actualDispatch := dispatch
			actualLabel := use
			if forceFlag {
				force, err := cmd.Flags().GetBool(flagForce)
				if err != nil {
					return err
				}
				if force {
					actualDispatch = func(c *cpclient.Client, ctx context.Context, name string) (cpclient.TaskAccepted, error) {
						return c.PoweroffVM(ctx, name)
					}
					actualLabel = "poweroff"
				}
			}

			accepted, err := actualDispatch(c, cmd.Context(), args[0])
			if err != nil {
				return classifyError(err)
			}

			printf(cmd, "%s task=%s status=%s\n", actualLabel, accepted.TaskID, accepted.Status)

			if !wait {
				return nil
			}
			taskID, err := parseTaskID(accepted.TaskID)
			if err != nil {
				return fmt.Errorf("request_failed: %v", err)
			}
			if err := waitForTask(cmd.Context(), cmd, c, taskID, timeout); err != nil {
				return classifyError(err)
			}
			printf(cmd, "vm %s complete task=%s\n", actualLabel, accepted.TaskID)
			return nil
		},
	}
	cmd.Flags().Bool(flagWait, false, "block until the task reaches terminal status")
	cmd.Flags().Duration(flagWaitTimeout, defaultWaitTO, "max time to wait when --wait is set")
	if forceFlag {
		cmd.Flags().Bool(flagForce, false,
			"dispatch to the poweroff endpoint instead of stop (hard shutdown)")
	}
	return cmd
}
