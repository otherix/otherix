// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package pool

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

func newGetCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <identifier>",
		Short: "Show a storage pool — flat by UUID, aggregated by name.",
		Long: `Dual-shape view:
  - UUID positional → flat per-node instance (one StoragePool row).
  - Name positional → aggregated PoolConceptView listing every per-node
    instance plus the cluster-default flag.

The CLI parses the positional locally к pick which roundtrip к make.
Behaviour mirrors the server's polymorphic GET — но keeping the
discrimination CLI-side avoids an unintended aggregated-view fetch on
a UUID input.`,
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

	if id, parseErr := uuid.Parse(identifier); parseErr == nil {
		instance, err := c.GetPoolByID(cmd.Context(), id)
		if err != nil {
			return classifyError(err)
		}
		return renderInstance(cmd, instance, format)
	}

	concept, err := c.GetPoolByName(cmd.Context(), identifier)
	if err != nil {
		return classifyError(err)
	}
	return renderConcept(cmd, concept, format)
}

func renderInstance(cmd *cobra.Command, p cpclient.Pool, format string) error {
	if format == "json" {
		raw, err := json.MarshalIndent(p, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
		return nil
	}
	printf(cmd, "id: %s\n", p.ID)
	printf(cmd, "name: %s\n", p.Name)
	printf(cmd, "node: %s\n", p.Node)
	printf(cmd, "type: %s\n", p.Type)
	printf(cmd, "path: %s\n", p.Path)
	printf(cmd, "available: %s\n", formatPoolAvailable(p.AvailableBytes, p.AvailableBytesEffective))
	printf(cmd, "capacity_bytes: %s\n", humanBytes(p.CapacityBytes))
	if p.ReportedAt != nil {
		printf(cmd, "reported_at: %s\n", *p.ReportedAt)
	}
	printf(cmd, "is_cluster_default: %t\n", p.IsClusterDefault)
	printPoolPressure(cmd, p)
	if p.ReconciliationStatus != "" {
		printf(cmd, "reconciliation_status: %s\n", p.ReconciliationStatus)
	}
	if p.LastReconciledAt != nil {
		printf(cmd, "last_reconciled_at: %s\n", *p.LastReconciledAt)
	}
	if p.ReconciliationError != nil && *p.ReconciliationError != "" {
		printf(cmd, "reconciliation_error: %s\n", *p.ReconciliationError)
	}
	printf(cmd, "created_at: %s\n", p.CreatedAt)
	printf(cmd, "updated_at: %s\n", p.UpdatedAt)
	return nil
}

// printPoolPressure renders the pool's disk_pressure condition.
// One-line per condition mirror of the node detail's `pressure:`
// section. Always emitted — operators can see "disk: ok" or "disk:
// active since 30m ago" parallel к node-level pressure rendering.
func printPoolPressure(cmd *cobra.Command, p cpclient.Pool) {
	printf(cmd, "pressure:\n")
	if p.DiskPressure != nil {
		printf(cmd, "  disk: active since %s ago (consecutive_count=%d)\n",
			humanAge(p.DiskPressure.Since), p.DiskPressure.ConsecutiveCount)
	} else {
		printf(cmd, "  disk: ok\n")
	}
}

func renderConcept(cmd *cobra.Command, v cpclient.PoolConceptView, format string) error {
	if format == "json" {
		raw, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
		return nil
	}
	printf(cmd, "name: %s\n", v.Name)
	printf(cmd, "type: %s\n", v.Type)
	printf(cmd, "is_cluster_default: %t\n", v.IsClusterDefault)
	if len(v.Instances) == 0 {
		printf(cmd, "instances: <none>\n")
		return nil
	}
	printf(cmd, "instances:\n")
	for _, inst := range v.Instances {
		printf(cmd, "  - node: %s\n", inst.Node)
		printf(cmd, "    id: %s\n", inst.ID)
		printf(cmd, "    path: %s\n", inst.Path)
		printf(cmd, "    available: %s\n", formatPoolAvailable(inst.AvailableBytes, inst.AvailableBytesEffective))
		if inst.ReconciliationStatus != "" {
			printf(cmd, "    reconciliation_status: %s\n", inst.ReconciliationStatus)
		}
		if inst.ReconciliationError != nil && *inst.ReconciliationError != "" {
			printf(cmd, "    reconciliation_error: %s\n", *inst.ReconciliationError)
		}
	}
	return nil
}
