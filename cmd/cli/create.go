// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"fmt"

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
			return renderSummary(cmd, "created", results)
		},
	}
	cmd.Flags().StringArrayVarP(&files, "filename", "f", nil, "manifest file path, or '-' for stdin (repeatable)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the plan without creating anything")
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
			results = append(results, docResult{kind: op.Kind, name: op.Name, err: err})
		case manifest.KindStoragePool:
			_, err := c.CreatePool(ctx, *op.Pool)
			results = append(results, docResult{kind: op.Kind, name: op.Name, note: "node " + op.Pool.Node, err: err})
		case manifest.KindVM:
			acc, err := c.CreateVM(ctx, *op.VM)
			note := ""
			if err == nil {
				note = "task " + acc.TaskID
			}
			results = append(results, docResult{kind: op.Kind, name: op.Name, note: note, err: err})
		}
	}
	return results
}
