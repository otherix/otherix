// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cloudinit"
	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

const (
	flagName             = "name"
	flagTemplate         = "template"
	flagPool             = "pool"
	flagNode             = "node"
	flagVCPUs            = "vcpus"
	flagMemoryMB         = "memory-mb"
	flagCloudInitPath    = "cloud-init"
	flagCloudInitDisable = "no-cloud-init"

	defaultVCPUs    = 2
	defaultMemoryMB = 2048
	defaultWaitTO   = 5 * time.Minute
)

func newCreateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new VM (async).",
		Long: `Submits a vm.create task and returns a task id immediately. Pass --wait
to block until the task reaches its terminal state. The VM uuid is
minted on the CP and reused by the agent.

--template is a template name (UUID literals rejected by the server
with 400 validation_failed). --pool accepts either a pool name or a
per-instance UUID literal (multi-instance carve-out). The CLI
forwards the raw string; the server resolves it.

Multi-instance pools + scheduler:
  - --pool is optional. When omitted, the server uses the cluster
    default-pool reference (configured via 'otherix cluster
    set-default-pool'). Missing default returns 400
    default_pool_not_set.
  - --node is an optional placement hint: when set, the scheduler
    pins the VM to exactly that node; mismatch (pool not present on
    the requested node) returns 409 pool_not_on_node.`,
		RunE: runCreate,
	}

	cmd.Flags().String(flagName, "", "VM name (required, globally unique)")
	cmd.Flags().String(flagTemplate, "", "template name or uuid (required)")
	cmd.Flags().String(flagPool, "", "storage pool name or uuid (optional; cluster default used when empty)")
	cmd.Flags().String(flagNode, "", "explicit placement hint — node name or uuid (optional)")
	cmd.Flags().Int(flagVCPUs, defaultVCPUs, "vCPU count (1..128)")
	cmd.Flags().Int(flagMemoryMB, defaultMemoryMB, "memory in MiB (128..524288)")
	cmd.Flags().String(flagCloudInitPath, "",
		"path to a `#cloud-config` YAML override; use '-' to read stdin. Mutually exclusive with --no-cloud-init.")
	cmd.Flags().Bool(flagCloudInitDisable, false,
		"explicitly disable cloud-init, ignoring any template pre-bake. Mutually exclusive with --cloud-init.")
	cmd.Flags().Bool(flagWait, false, "block until the task reaches terminal status")
	cmd.Flags().Duration(flagWaitTimeout, defaultWaitTO, "max time to wait when --wait is set")

	return cmd
}

// createFlags collects the strongly-typed inputs runCreate needs.
// Extracting the parse step keeps runCreate's cyclomatic complexity
// inside the linter cap.
type createFlags struct {
	name              string
	template          string
	pool              string
	node              string
	vcpus             int
	memoryMB          int
	cloudInitUserData *string
	cloudInitDisabled bool
	wait              bool
	timeout           time.Duration
}

func parseCreateFlags(cmd *cobra.Command) (createFlags, error) {
	var f createFlags
	var err error
	if f.name, err = cmd.Flags().GetString(flagName); err != nil {
		return f, err
	}
	if f.name == "" {
		return f, fmt.Errorf("--%s is required", flagName)
	}
	if f.template, err = requireStringFlag(cmd, flagTemplate); err != nil {
		return f, err
	}
	if f.pool, err = cmd.Flags().GetString(flagPool); err != nil {
		return f, err
	}
	if f.node, err = cmd.Flags().GetString(flagNode); err != nil {
		return f, err
	}
	if f.vcpus, err = cmd.Flags().GetInt(flagVCPUs); err != nil {
		return f, err
	}
	if f.memoryMB, err = cmd.Flags().GetInt(flagMemoryMB); err != nil {
		return f, err
	}
	if f.cloudInitDisabled, err = cmd.Flags().GetBool(flagCloudInitDisable); err != nil {
		return f, err
	}
	if f.cloudInitUserData, err = readCloudInitFlag(cmd, flagCloudInitPath); err != nil {
		return f, err
	}
	if f.cloudInitUserData != nil && f.cloudInitDisabled {
		return f, fmt.Errorf("--%s and --%s are mutually exclusive", flagCloudInitPath, flagCloudInitDisable)
	}
	if f.wait, err = cmd.Flags().GetBool(flagWait); err != nil {
		return f, err
	}
	if f.timeout, err = cmd.Flags().GetDuration(flagWaitTimeout); err != nil {
		return f, err
	}
	return f, nil
}

// readCloudInitFlag turns the `--cloud-init=<path|->` flag into the
// resolved YAML content. Empty flag returns nil; non-empty reads
// (file or stdin) and validates (best-effort). Mirrors the template
// CLI helper of the same name; both surfaces consume the shared
// cloudinit package so the contract (warnings to stderr, parse errors
// bubble up) stays uniform across resources.
func readCloudInitFlag(cmd *cobra.Command, name string) (*string, error) {
	raw, err := cmd.Flags().GetString(name)
	if err != nil {
		return nil, err
	}
	if raw == "" {
		return nil, nil
	}
	data, err := cloudinit.ReadSource(raw)
	if err != nil {
		return nil, fmt.Errorf("--%s: %w", name, err)
	}
	warnings, err := cloudinit.Validate(data)
	if err != nil {
		return nil, fmt.Errorf("--%s: %w", name, err)
	}
	stderr := cmd.ErrOrStderr()
	if stderr == nil {
		stderr = os.Stderr
	}
	for _, w := range warnings {
		_, _ = fmt.Fprintf(stderr, "warning: --%s: %s\n", name, w)
	}
	out := string(data)
	return &out, nil
}

func runCreate(cmd *cobra.Command, _ []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	f, err := parseCreateFlags(cmd)
	if err != nil {
		return err
	}

	req := cpclient.CreateVMRequest{
		Name:              f.name,
		Template:          f.template,
		Pool:              f.pool,
		VCPUs:             f.vcpus,
		MemoryMB:          f.memoryMB,
		UserData:          f.cloudInitUserData,
		CloudInitDisabled: f.cloudInitDisabled,
	}
	if f.node != "" {
		req.Node = &f.node
	}
	accepted, err := c.CreateVM(cmd.Context(), req)
	if err != nil {
		return classifyError(err)
	}

	printf(cmd, "created task=%s status=%s\n", accepted.TaskID, accepted.Status)

	if !f.wait {
		return nil
	}

	taskID, err := parseTaskID(accepted.TaskID)
	if err != nil {
		return fmt.Errorf("request_failed: %v", err)
	}

	if err := waitForTask(cmd.Context(), cmd, c, taskID, f.timeout); err != nil {
		return classifyError(err)
	}
	printf(cmd, "vm running task=%s\n", accepted.TaskID)
	return nil
}
