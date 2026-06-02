// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

func newGetCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <vm>",
		Short: "Show a VM's projection.",
		Long: `Fetches the VM's projected view from the CP. The positional is a
VM name (UUID literals rejected by the server with 400
validation_failed). The status field is computed at read time from
(vms.deleted_at, vm_runtime.phase) — see the projectStatus truth
table in internal/api/handlers/vms/projection.go.`,
		Args: cobra.ExactArgs(1),
		RunE: runGet,
	}
	cmd.Flags().String(flagOutput, "text", "output format: text|json")
	return cmd
}

func runGet(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	identifier := args[0]
	format, err := outputFormat(cmd, "text")
	if err != nil {
		return err
	}

	vm, raw, err := c.VM(cmd.Context(), identifier)
	if err != nil {
		return classifyError(err)
	}

	if format == "json" {
		return printJSON(cmd, raw)
	}
	printVMText(cmd, vm)
	return nil
}

// printVMText renders the operator-friendly multi-line key=value form
// (one field per line). Nullable fields print "<unset>" so the
// formatter is unambiguous under empty templates / unscheduled VMs.
// Referenced resources are surfaced by name — the wire field tags are
// `template` / `pool` / `node`, and so are the labels below.
func printVMText(cmd *cobra.Command, vm cpclient.VM) {
	printf(cmd, "id: %s\n", vm.ID)
	printf(cmd, "name: %s\n", vm.Name)
	printf(cmd, "owner_id: %s\n", vm.OwnerID)
	printf(cmd, "template: %s\n", strOrUnset(vm.Template))
	printf(cmd, "pool: %s\n", vm.Pool)
	printf(cmd, "node: %s\n", strOrUnset(vm.Node))
	printf(cmd, "architecture: %s\n", vm.Architecture)
	printf(cmd, "vcpus: %d\n", vm.VCPUs)
	printf(cmd, "memory_mb: %d\n", vm.MemoryMB)
	printf(cmd, "status: %s\n", vm.Status)
	printf(cmd, "desired_phase: %s\n", vm.DesiredPhase)
	printf(cmd, "created_at: %s\n", vm.CreatedAt)
	printf(cmd, "updated_at: %s\n", vm.UpdatedAt)
}

func strOrUnset(s *string) string {
	if s == nil {
		return "<unset>"
	}
	return *s
}
