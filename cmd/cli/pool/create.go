// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package pool

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

const (
	flagCreateNode = "node"
	flagCreatePath = "path"
	flagCreateType = "type"
	flagCreateWait = "wait"

	defaultPoolType = "local_dir"

	// waitPoolPollInterval / waitPoolTimeout control the --wait
	// reconciliation poll. Interval long enough to keep CP load low,
	// short enough to feel responsive on the typical 30 s heartbeat
	// cadence. Timeout sized to 2× heartbeat for slack.
	waitPoolPollInterval = 2 * time.Second
	waitPoolTimeout      = 60 * time.Second
)

// newCreateCommand returns the `otherix pool create` cobra command.
// One invocation registers one (name, node) row — multi-instance is
// achieved by re-running the command per node. Operators can compose
// batches in shell / Make if needed; no batch shorthand is offered
// to keep the per-call semantics observable.
func newCreateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Register a storage pool on a node (admin-only).",
		Long: `Submits POST /v1/storage-pools. Required permission:
storage_pool:manage (admin role). The CLI accepts a node name in
--node (UUID literals rejected by the server with 400
validation_failed). Multi-instance: one invocation registers one
(name, node) row — the same pool name may live on multiple nodes by
re-running the command per node.

The command is NOT idempotent at the CLI surface: a duplicate (name,
node) returns ErrPoolExists and the CLI surfaces a clear error.
Operators driving seed scripts that need re-run safety should pre-check
existence via 'otherix pool get <name>' before invoking create.

Example:
  otherix pool create pool-dev \
    --node node-dev \
    --path /var/lib/otherix/pools/default

  # Explicit type (only local_dir is supported today):
  otherix pool create pool-dev \
    --node node-dev \
    --type local_dir \
    --path /var/lib/otherix/pools/default`,
		Args: cobra.ExactArgs(1),
		RunE: runCreate,
	}
	cmd.Flags().String(flagCreateNode, "", "owning node name — required")
	cmd.Flags().String(flagCreatePath, "", "filesystem path on the owning node — required")
	cmd.Flags().String(flagCreateType, defaultPoolType, "pool type (local_dir)")
	cmd.Flags().Bool(flagCreateWait, false, "poll until reconciliation_status reaches 'ready' or 'failed' (60s timeout)")
	cmd.Flags().String(flagOutput, "text", "output format: text|json")
	cmd.Flags().Bool(flagShowIDs, false, "include the pool UUID in the text output")

	_ = cmd.MarkFlagRequired(flagCreateNode)
	_ = cmd.MarkFlagRequired(flagCreatePath)
	return cmd
}

func runCreate(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	name := args[0]
	if name == "" {
		return errors.New("pool name is required")
	}
	node, err := cmd.Flags().GetString(flagCreateNode)
	if err != nil {
		return err
	}
	path, err := cmd.Flags().GetString(flagCreatePath)
	if err != nil {
		return err
	}
	poolType, err := cmd.Flags().GetString(flagCreateType)
	if err != nil {
		return err
	}
	if poolType == "" {
		poolType = defaultPoolType
	}
	format, err := outputFormat(cmd, "text")
	if err != nil {
		return err
	}
	showIDs, _ := cmd.Flags().GetBool(flagShowIDs)
	wait, _ := cmd.Flags().GetBool(flagCreateWait)

	created, err := c.CreatePool(cmd.Context(), cpclient.CreatePoolRequest{
		Node: node,
		Name: name,
		Type: poolType,
		Path: path,
	})
	if err != nil {
		if errors.Is(err, cpclient.ErrPoolExists) {
			return fmt.Errorf("pool %s already exists on node %s — use 'otherix pool get %s' to inspect, or pick a different name",
				name, node, name)
		}
		return classifyError(err)
	}

	final := created
	if wait {
		printf(cmd, "waiting for agent reconciliation (timeout %s)…\n", waitPoolTimeout)
		final, err = waitForReady(cmd, c, created)
		if err != nil {
			return err
		}
	}

	return renderCreateOutput(cmd, final, format, showIDs, wait)
}

// renderCreateOutput writes the JSON or text representation of the
// (possibly waited-for) pool view. Split from runCreate to keep its
// cyclomatic complexity in check.
func renderCreateOutput(cmd *cobra.Command, p cpclient.Pool, format string, showIDs, wait bool) error {
	if format == "json" {
		raw, err := json.MarshalIndent(p, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
		return nil
	}
	printf(cmd, "pool %s created on node %s\n", p.Name, p.Node)
	if showIDs {
		printf(cmd, "id: %s\n", p.ID)
	}
	printf(cmd, "type: %s\n", p.Type)
	printf(cmd, "path: %s\n", p.Path)
	if wait || p.ReconciliationStatus != "" {
		printf(cmd, "reconciliation_status: %s\n", p.ReconciliationStatus)
		if p.LastReconciledAt != nil {
			printf(cmd, "last_reconciled_at: %s\n", *p.LastReconciledAt)
		}
		if p.ReconciliationError != nil && *p.ReconciliationError != "" {
			printf(cmd, "reconciliation_error: %s\n", *p.ReconciliationError)
		}
	}
	return nil
}

// waitForReady polls GET /v1/storage-pools/{id} until
// reconciliation_status reaches `ready` (returns the latest Pool view)
// or `failed` (returns the failed view + a non-nil error so callers
// surface a non-zero exit). Times out after waitPoolTimeout with
// a user-facing error noting the operator can re-poll manually via
// `otherix pool get`.
func waitForReady(cmd *cobra.Command, c *cpclient.Client, initial cpclient.Pool) (cpclient.Pool, error) {
	ctx := cmd.Context()
	deadline := time.Now().Add(waitPoolTimeout)
	current := initial
	for {
		switch current.ReconciliationStatus {
		case "ready":
			return current, nil
		case "failed":
			msg := "unknown"
			if current.ReconciliationError != nil {
				msg = *current.ReconciliationError
			}
			return current, fmt.Errorf("pool reconciliation failed: %s", msg)
		}
		if time.Now().After(deadline) {
			return current, fmt.Errorf("pool reconciliation did not reach 'ready' within %s — last status: %q (retry with 'otherix pool get %s' to keep polling)",
				waitPoolTimeout, current.ReconciliationStatus, current.Name)
		}
		select {
		case <-ctx.Done():
			return current, ctx.Err()
		case <-time.After(waitPoolPollInterval):
		}
		refreshed, _, err := c.GetPoolByID(ctx, current.ID)
		if err != nil {
			return current, fmt.Errorf("poll pool: %v", err)
		}
		current = refreshed
	}
}
