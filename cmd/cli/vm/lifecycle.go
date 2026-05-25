// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// newPauseCommand wires `otherix vm pause <vm>` — synchronous per the
// CP spec. The handler issues QMP `stop` against the pinned agent
// and returns the refreshed VM with status=paused.
func newPauseCommand() *cobra.Command {
	return newLifecycleCommand(
		"pause",
		"Pause a running VM (QMP stop, synchronous).",
		`Sends POST /v1/vms/<vm>/pause to the Control Plane. The CP forwards
to the pinned agent, which issues QMP `+"`stop`"+` to the QEMU process —
vCPUs freeze, memory stays resident. Synchronous: the response
carries the refreshed VM view with status=paused. desired_phase is
unchanged. Re-pausing an already-paused VM returns 409 conflict.`,
		func(c *cpclient.Client, ctx context.Context, name string) (cpclient.VM, error) {
			return c.PauseVM(ctx, name)
		},
	)
}

// newResumeCommand wires `otherix vm resume <vm>` — synchronous QMP
// `cont` dispatch.
func newResumeCommand() *cobra.Command {
	return newLifecycleCommand(
		"resume",
		"Resume a paused VM (QMP cont, synchronous).",
		`Sends POST /v1/vms/<vm>/resume to the Control Plane. The CP forwards
to the pinned agent, which issues QMP `+"`cont`"+` to the QEMU process —
vCPUs unfreeze and the guest continues from where Pause left it.
Synchronous: the response carries the refreshed VM view with
status=running. Re-resuming an already-running VM returns 409
conflict.`,
		func(c *cpclient.Client, ctx context.Context, name string) (cpclient.VM, error) {
			return c.ResumeVM(ctx, name)
		},
	)
}

// newResetCommand wires `otherix vm reset <vm>` — synchronous QMP
// `system_reset` dispatch.
func newResetCommand() *cobra.Command {
	return newLifecycleCommand(
		"reset",
		"Hard reset a running VM (QMP system_reset, synchronous).",
		`Sends POST /v1/vms/<vm>/reset to the Control Plane. The CP forwards
to the pinned agent, which issues QMP `+"`system_reset`"+` — equivalent
to the physical reset button. The QEMU process keeps running and the
guest OS reboots without notification, so an in-flight write may surface
inconsistencies. Synchronous: the response carries the refreshed VM
view with status=running (the runtime identity is preserved — operators
detect the reboot through guest uptime, not via CP state).`,
		func(c *cpclient.Client, ctx context.Context, name string) (cpclient.VM, error) {
			return c.ResetVM(ctx, name)
		},
	)
}

// newLifecycleCommand is the shared builder for the three sync
// lifecycle subcommands. They differ only in the cpclient method
// they invoke; everything else (flag wiring, output format,
// error mapping) is identical.
func newLifecycleCommand(
	use, short, long string,
	dispatch func(*cpclient.Client, context.Context, string) (cpclient.VM, error),
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
			format, err := outputFormat(cmd, "text")
			if err != nil {
				return err
			}
			vm, err := dispatch(c, cmd.Context(), args[0])
			if err != nil {
				return classifyError(err)
			}
			switch format {
			case "json":
				raw, err := json.MarshalIndent(vm, "", "  ")
				if err != nil {
					return fmt.Errorf("marshal json: %v", err)
				}
				printf(cmd, "%s\n", raw)
			default:
				printVMText(cmd, vm)
			}
			return nil
		},
	}
	cmd.Flags().String(flagOutput, "text", "output format: text|json")
	return cmd
}
