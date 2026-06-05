// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
	"github.com/otherix/otherix/cmd/cli/internal/manifest"
)

// newManifestDeleteCmd builds `otherix delete -f`. It reads manifests,
// resolves each document's identity, and deletes resources in reverse
// create order (VM -> StoragePool -> Network) so dependents go first. A
// StoragePool with nodeList expands to one delete per node. Best-effort
// with a per-document summary and non-zero exit on any failure.
func newManifestDeleteCmd() *cobra.Command {
	var files []string
	var force, dryRun bool
	cmd := &cobra.Command{
		Use:   "delete -f FILE [-f FILE ...]",
		Short: "Delete resources named by YAML manifests (multi-document).",
		Long: `Reads otherix/v1 manifests and deletes the named resources in
reverse create order (VM -> StoragePool -> Network). A StoragePool with
spec.nodeList deletes the instance on each listed node. Existing delete
blockers (e.g. a network still in use) are reported and skipped, never
forced. --force skips the confirmation prompt; --dry-run prints the
plan without deleting.

Example:
  otherix delete -f cluster.yaml --force`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			docs, err := readManifestDocs(cmd, files)
			if err != nil {
				return err
			}
			targets, err := manifest.BuildDeletePlan(docs)
			if err != nil {
				return err
			}
			if dryRun {
				for _, t := range targets {
					label := t.Kind + "/" + t.Name
					if t.PoolNode != "" {
						label += " (node " + t.PoolNode + ")"
					}
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "would delete %s\n", label)
				}
				return nil
			}
			if !force && stdinIsTTY() {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "delete %d resource(s) from the manifest? [y/N]: ", len(targets))
				if !readYes(cmd) {
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), "aborted")
					return nil
				}
			}
			c, err := cliauth.BuildClient(cmd)
			if err != nil {
				return err
			}
			results := runDeletePlan(cmd, c, targets)
			return renderSummary(cmd, "deleted", results)
		},
	}
	cmd.Flags().StringArrayVarP(&files, "filename", "f", nil, "manifest file path, or '-' for stdin (repeatable)")
	cmd.Flags().BoolVar(&force, "force", false, "skip the confirmation prompt")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the plan without deleting anything")
	return cmd
}

// runDeletePlan deletes each target in order, resolving StoragePool
// (node, name) to its instance UUID first. Failures are recorded and
// the loop continues.
func runDeletePlan(cmd *cobra.Command, c *cpclient.Client, targets []manifest.DeleteTarget) []docResult {
	ctx := cmd.Context()
	results := make([]docResult, 0, len(targets))
	for _, t := range targets {
		switch t.Kind {
		case manifest.KindVM:
			_, err := c.DeleteVM(ctx, t.Name)
			results = append(results, docResult{kind: t.Kind, name: t.Name, err: err})
		case manifest.KindNetwork:
			err := c.DeleteNetwork(ctx, t.Name)
			results = append(results, docResult{kind: t.Kind, name: t.Name, err: err})
		case manifest.KindStoragePool:
			results = append(results, deletePoolInstance(ctx, c, t))
		}
	}
	return results
}

// deletePoolInstance resolves a (node, name) pool instance to its UUID
// and deletes it. A name that does not resolve to exactly one instance
// on the node is an error (act under uncertainty -> inaction).
func deletePoolInstance(ctx context.Context, c *cpclient.Client, t manifest.DeleteTarget) docResult {
	res := docResult{kind: t.Kind, name: t.Name, note: "node " + t.PoolNode}
	concept, _, err := c.GetPoolByName(ctx, t.Name)
	if err != nil {
		res.err = err
		return res
	}
	var id string
	for _, inst := range concept.Instances {
		if inst.Node == t.PoolNode {
			id = inst.ID.String()
			break
		}
	}
	if id == "" {
		res.err = fmt.Errorf("no instance on node %q", t.PoolNode)
		return res
	}
	res.err = c.DeletePool(ctx, id)
	return res
}

// stdinIsTTY reports whether stdin is a terminal. Tests pipe stdin so
// this returns false and the confirmation prompt is skipped.
func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// readYes reads a line from stdin and returns true on a y/Y prefix.
func readYes(cmd *cobra.Command) bool {
	r := bufio.NewReader(cmd.InOrStdin())
	line, err := r.ReadString('\n')
	if err != nil {
		return false
	}
	line = strings.TrimSpace(line)
	return line != "" && (line[0] == 'y' || line[0] == 'Y')
}
