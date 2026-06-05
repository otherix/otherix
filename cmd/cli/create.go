// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
	"github.com/otherix/otherix/cmd/cli/internal/manifest"
)

// newManifestCreateCmd builds `otherix create -f`. It reads one or more
// YAML manifests, validates and orders the documents (Network ->
// StoragePool -> VM), and creates each resource via the existing REST
// methods. Best-effort: a failed document does not stop the rest, and
// the command exits non-zero if any document failed.
func newManifestCreateCmd() *cobra.Command {
	var files []string
	var dryRun bool
	var wait bool
	var waitTimeout time.Duration
	cmd := &cobra.Command{
		Use:   "create -f FILE [-f FILE ...]",
		Short: "Create resources from YAML manifests (multi-document).",
		Long: `Reads otherix/v1 manifests (Network, StoragePool, VM), validates
and orders them, and creates each resource. Documents are separated by
'---'. Resources are created Network -> StoragePool -> VM so name
references resolve. A StoragePool with spec.nodeList is expanded to one
instance per node. Best-effort: each document is reported independently
and the command exits non-zero if any failed.

Example:
  otherix create -f cluster.yaml
  otherix create -f net.yaml -f pool.yaml --dry-run`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			docs, err := readManifestDocs(cmd, files)
			if err != nil {
				return err
			}
			plan, err := manifest.BuildCreatePlan(docs)
			if err != nil {
				return err
			}
			if len(plan) == 0 {
				return errors.New("manifest contained no resources")
			}
			if err := validateManifestCloudInit(cmd, plan); err != nil {
				return err
			}
			if dryRun {
				for _, op := range plan {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "would create %s/%s\n", op.Kind, op.Name)
				}
				return nil
			}
			c, err := cliauth.BuildClient(cmd)
			if err != nil {
				return err
			}
			results := runCreatePlan(cmd, c, plan)
			if wait {
				waitForCreated(cmd, c, results, waitTimeout)
			}
			return renderSummary(cmd, "created", results)
		},
	}
	cmd.Flags().StringArrayVarP(&files, "filename", "f", nil, "manifest file path, or '-' for stdin (repeatable)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the plan without creating anything")
	cmd.Flags().BoolVar(&wait, "wait", false, "block until every async resource (VM tasks, pool reconciliation) is ready")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", 5*time.Minute, "max time to wait when --wait is set")
	return cmd
}

// runCreatePlan executes each create op in order, collecting per-op
// results. A failed op is recorded and the loop continues.
func runCreatePlan(cmd *cobra.Command, c *cpclient.Client, plan []manifest.CreateOp) []docResult {
	ctx := cmd.Context()
	results := make([]docResult, 0, len(plan))
	for _, op := range plan {
		switch op.Kind {
		case manifest.KindNetwork:
			_, err := c.CreateNetwork(ctx, *op.Network)
			results = append(results, docResult{kind: op.Kind, name: op.Name, committed: err == nil, err: fanoutErr(err)})
		case manifest.KindStoragePool:
			p, err := c.CreatePool(ctx, *op.Pool)
			res := docResult{kind: op.Kind, name: op.Name, note: "node " + op.Pool.Node, committed: err == nil, err: fanoutErr(err)}
			if err == nil {
				res.poolID = p.ID.String()
			}
			results = append(results, res)
		case manifest.KindVM:
			acc, err := c.CreateVM(ctx, *op.VM)
			res := docResult{kind: op.Kind, name: op.Name, committed: err == nil, err: fanoutErr(err)}
			if err == nil {
				res.taskID = acc.TaskID
				res.note = "task " + acc.TaskID
			}
			results = append(results, res)
		}
	}
	return results
}
