// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
	"github.com/otherix/otherix/cmd/cli/internal/manifest"
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
	cmd.Flags().StringP(flagOutput, "o", "text", "output format: text|json|yaml")
	return cmd
}

func runGet(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	identifier := args[0]
	format, err := outputFormat(cmd, "text", "yaml")
	if err != nil {
		return err
	}

	vm, raw, err := c.VM(cmd.Context(), identifier)
	if err != nil {
		return classifyError(err)
	}

	if format == "yaml" {
		out, err := manifest.ProjectVM(vm)
		if err != nil {
			return err
		}
		printf(cmd, "%s", out)
		return nil
	}
	if format == "json" {
		return printJSON(cmd, raw)
	}
	printVMText(cmd, vm)
	return nil
}

// printVMText renders the operator-friendly multi-line key=value form
// (one field per line). Nullable fields print "<unset>" so the
// formatter is unambiguous under unscheduled VMs. Referenced resources
// are surfaced by name — the wire field tags are `pool` / `node`, and
// so are the labels below. The image source (`image_url` / `format`)
// is rendered inline since the template entity is gone. Owner renders
// as the resolved display_name when the server returned one (caller
// holds user:read); otherwise the raw owner_id UUID is shown.
func printVMText(cmd *cobra.Command, vm cpclient.VM) {
	printf(cmd, "id: %s\n", vm.ID)
	printf(cmd, "name: %s\n", vm.Name)
	if vm.Owner != nil {
		printf(cmd, "owner: %s\n", *vm.Owner)
	} else {
		printf(cmd, "owner_id: %s\n", vm.OwnerID)
	}
	printf(cmd, "image_url: %s\n", vm.ImageURL)
	if vm.ImageSHA256 != "" {
		printf(cmd, "image_sha256: %s\n", vm.ImageSHA256)
	}
	printf(cmd, "format: %s\n", vm.Format)
	printf(cmd, "pool: %s\n", vm.Pool)
	printf(cmd, "node: %s\n", strOrUnset(vm.Node))
	printf(cmd, "architecture: %s\n", vm.Architecture)
	printf(cmd, "vcpus: %d\n", vm.VCPUs)
	printf(cmd, "memory_mb: %d\n", vm.MemoryMB)
	printf(cmd, "status: %s\n", formatVMStatus(vm.Status))
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

// formatVMStatus renders the nested status object for the text output.
// A settled VM shows just its phase (e.g. "running"); a VM still pending
// placement appends the scheduling reason/message the CP reconcile loop
// recorded, e.g. `pending (pool_not_ready: pool "default" not ready)`,
// so operators see why it has not been bound yet without reaching for
// --output json.
func formatVMStatus(s cpclient.VMStatus) string {
	if s.Phase != "pending" {
		return s.Phase
	}
	switch {
	case s.Reason != "" && s.Message != "":
		return fmt.Sprintf("pending (%s: %s)", s.Reason, s.Message)
	case s.Reason != "":
		return fmt.Sprintf("pending (%s)", s.Reason)
	default:
		return "pending"
	}
}
