// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package node

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

func newGetCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <node>",
		Short: "Show a node's projection.",
		Long: `Fetches the node view from the CP. The positional is a node name
(UUID literals rejected by the server with 400 validation_failed).
The text renderer
prints only fields the server populated: admin / operator callers
get migration capability and hardware inventory; developer / viewer
callers see the reduced NodeSummary shape.`,
		Args: cobra.ExactArgs(1),
		RunE: runGet,
	}
	cmd.Flags().String(flagOutput, "text", "output format: text|json")
	cmd.Flags().Bool(flagShowIDs, false, "include the node UUID in text output")
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
	showIDs, _ := cmd.Flags().GetBool(flagShowIDs)

	n, err := c.GetNode(cmd.Context(), identifier)
	if err != nil {
		return classifyError(err)
	}

	switch format {
	case "json":
		raw, err := json.MarshalIndent(n, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
	default:
		printNodeText(cmd, n, showIDs)
	}
	return nil
}

// printNodeText renders only fields the wire envelope populated.
// nil pointer / empty string / nil map fields are skipped so the
// reduced NodeSummary projection (developer/viewer) renders cleanly
// without empty-key noise. The body is split into per-section helpers
// to keep cyclomatic complexity below the linter cap.
func printNodeText(cmd *cobra.Command, n cpclient.Node, showIDs bool) {
	if showIDs {
		printf(cmd, "id: %s\n", n.ID)
	}
	printf(cmd, "name: %s\n", n.Name)
	printf(cmd, "architecture: %s\n", n.Architecture)
	printf(cmd, "status: %s\n", n.Status)
	if n.CordonedAt != nil {
		printf(cmd, "cordoned_at: %s\n", *n.CordonedAt)
	}
	printNodeLabels(cmd, n.Labels)
	printNodeMigration(cmd, n)
	printNodeHardware(cmd, n)
	printNodePressure(cmd, n)
	printNodeAgent(cmd, n)
	printNodeWireguard(cmd, n)
	printf(cmd, "created_at: %s\n", n.CreatedAt)
	if n.UpdatedAt != nil {
		printf(cmd, "updated_at: %s\n", *n.UpdatedAt)
	}
}

func printNodeLabels(cmd *cobra.Command, labels map[string]string) {
	if len(labels) == 0 {
		return
	}
	printf(cmd, "labels:\n")
	for k, v := range labels {
		printf(cmd, "  %s: %s\n", k, v)
	}
}

func printNodeMigration(cmd *cobra.Command, n cpclient.Node) {
	if n.AdvertisedEndpoint != nil {
		printf(cmd, "advertised_endpoint: %s\n", *n.AdvertisedEndpoint)
	}
	if n.Migration != nil {
		printf(cmd, "migration_host: %s\n", n.Migration.Host)
		printf(cmd, "migration_port_range: %d-%d\n",
			n.Migration.PortRangeStart, n.Migration.PortRangeEnd)
	}
}

func printNodeHardware(cmd *cobra.Command, n cpclient.Node) {
	if n.CPUCoresTotal != nil {
		printf(cmd, "cpu_cores: %s\n", formatCoresUsage(n.CPUCoresTotal, n.CPUCoresAvailable, n.CPUCoresEffective))
	}
	if n.CPUModel != nil && *n.CPUModel != "" {
		printf(cmd, "cpu_model: %s\n", *n.CPUModel)
	}
	if n.MemoryTotalMiB != nil {
		printf(cmd, "memory: %s\n", formatMemoryUsage(n.MemoryTotalMiB, n.MemoryAvailableMiB, n.MemoryEffectiveMiB))
	}
	if n.SystemDiskTotalBytes != nil {
		printf(cmd, "system_disk: %s\n", formatSystemDiskUsage(n.SystemDiskTotalBytes, n.SystemDiskAvailableBytes))
	}
	if n.KernelVersion != nil && *n.KernelVersion != "" {
		printf(cmd, "kernel_version: %s\n", *n.KernelVersion)
	}
	if n.QEMUVersion != nil && *n.QEMUVersion != "" {
		printf(cmd, "qemu_version: %s\n", *n.QEMUVersion)
	}
}

// formatSystemDiskUsage renders "used N / M GiB" with the percentage
// available trailing in parens. Symmetric to formatMemoryUsage but
// stays in bytes. NULL available falls to a bare-total form for the
// pre-heartbeat case.
func formatSystemDiskUsage(total, available *int64) string {
	if total == nil {
		return "-"
	}
	totalBytes := humanBytes(total)
	if available == nil {
		return totalBytes
	}
	used := *total - *available
	if used < 0 {
		used = 0
	}
	usedBytes := humanBytes(&used)
	pct := 0.0
	if *total > 0 {
		pct = float64(*available) * 100.0 / float64(*total)
	}
	return fmt.Sprintf("used %s / %s (%.1f%% free)", usedBytes, totalBytes, pct)
}

// printNodePressure renders the pressure-conditions section of the
// detail view. One line per node-scoped pressure type: memory +
// system_disk. Pool disk pressure is
// rendered through `otherix pool get` — a node hosting a pressured
// pool stays "ready" from the node's perspective. Suppressed entirely
// on unreachable nodes — stale heartbeat data would mislead the
// operator.
func printNodePressure(cmd *cobra.Command, n cpclient.Node) {
	if n.Status == "unreachable" {
		return
	}
	printf(cmd, "pressure:\n")
	printNodePressureLine(cmd, "memory", n.MemoryPressure)
	printNodePressureLine(cmd, "system_disk", n.SystemDiskPressure)
}

// printNodePressureLine emits one indented `<type>: ...` row. Active
// pressure prints since-age + count; inactive prints "ok". Shared
// between memory and system_disk because the two conditions render
// identically.
func printNodePressureLine(cmd *cobra.Command, label string, p *cpclient.PressureCondition) {
	if p == nil {
		printf(cmd, "  %s: ok\n", label)
		return
	}
	printf(cmd, "  %s: active since %s ago (consecutive_count=%d)\n",
		label, humanAge(p.Since), p.ConsecutiveCount)
}

// printNodeWireguard renders the WG underlay fabric block when present: the
// node's own overlay identity then a peers table. Omitted cleanly when the
// server did not populate the block (developer/viewer, or pre-WG-report node).
func printNodeWireguard(cmd *cobra.Command, n cpclient.Node) {
	if n.WireGuard == nil {
		return
	}
	wg := n.WireGuard
	printf(cmd, "wireguard:\n")
	printf(cmd, "  overlay_ip: %s\n", wg.OverlayIP)
	printf(cmd, "  public_key: %s\n", wg.PublicKey)
	printf(cmd, "  listen_port: %d\n", wg.ListenPort)
	if wg.Endpoint != "" {
		printf(cmd, "  endpoint: %s\n", wg.Endpoint)
	}
	if wg.Status != "" {
		printf(cmd, "  reconciliation_status: %s\n", wg.Status)
	}
	if wg.Status == "failed" && wg.Error != nil {
		printf(cmd, "  reconciliation_error: %s\n", *wg.Error)
	}
	if len(wg.Peers) == 0 {
		printf(cmd, "  peers: none\n")
		return
	}
	printf(cmd, "  peers:\n")
	for _, p := range wg.Peers {
		name := p.NodeID
		if p.NodeName != nil && *p.NodeName != "" {
			name = *p.NodeName
		}
		state := "down"
		if p.Established {
			state = "established"
		}
		printf(cmd, "    %s  %s  %s\n", name, p.OverlayIP, state)
	}
}

func printNodeAgent(cmd *cobra.Command, n cpclient.Node) {
	if n.AgentVersion != nil && *n.AgentVersion != "" {
		printf(cmd, "agent_version: %s\n", *n.AgentVersion)
	}
	if n.LastHeartbeatAt != nil {
		printf(cmd, "last_heartbeat_at: %s (%s)\n",
			*n.LastHeartbeatAt, humanAge(*n.LastHeartbeatAt))
	}
}

// formatCoresUsage renders "used N/M cores" (raw heartbeat) optionally
// followed by "(effective N free)" when the view-reported effective
// availability disagrees with the raw available — a pending VM is
// pinned but not yet observed by the agent. When raw available is
// NULL the row is pre-heartbeat and we render the bare
// total. When effective equals raw available, the suffix is omitted to
// reduce noise.
func formatCoresUsage(total, available, effective *int32) string {
	if total == nil {
		return "-"
	}
	if available == nil {
		return fmt.Sprintf("%d cores", *total)
	}
	used := *total - *available
	if used < 0 {
		used = 0
	}
	base := fmt.Sprintf("used %d/%d cores", used, *total)
	if effective != nil && *effective != *available {
		base += fmt.Sprintf(" (effective %d free)", *effective)
	}
	return base
}

// formatMemoryUsage renders "used N/M MiB" symmetrical to
// formatCoresUsage, with the same effective-divergence suffix.
func formatMemoryUsage(total, available, effective *int64) string {
	if total == nil {
		return "-"
	}
	if available == nil {
		return fmt.Sprintf("%d MiB", *total)
	}
	used := *total - *available
	if used < 0 {
		used = 0
	}
	base := fmt.Sprintf("used %d/%d MiB", used, *total)
	if effective != nil && *effective != *available {
		base += fmt.Sprintf(" (effective %d MiB free)", *effective)
	}
	return base
}
