// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cluster

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

func newGetDefaultArtifactPoolCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get-default-artifact-pool",
		Short: "Show the cluster default artifact pool reference.",
		Long: `Fetches /v1/cluster/default-artifact-pool. When unset, prints an
actionable informational line to stdout and exits 0 (parseable by
tooling that distinguishes set/unset without inspecting exit codes).`,
		RunE: runGetDefaultArtifactPool,
	}
	cmd.Flags().StringP(flagOutput, "o", "text", "output format: text|json")
	return cmd
}

func runGetDefaultArtifactPool(cmd *cobra.Command, _ []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	format, err := outputFormat(cmd, "text")
	if err != nil {
		return err
	}

	got, err := c.GetClusterDefaultArtifactPool(cmd.Context())
	if err != nil {
		return classifyError(err)
	}

	if format == "json" {
		if got == nil {
			printf(cmd, "null\n")
			return nil
		}
		raw, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
		return nil
	}

	if got == nil {
		printf(cmd, "no default artifact pool configured (run 'otherix cluster set-default-artifact-pool <name>' to configure)\n")
		return nil
	}
	printf(cmd, "default-artifact-pool: %s\n", got.Name)
	return nil
}

func newSetDefaultArtifactPoolCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set-default-artifact-pool <name>",
		Short: "Set the cluster default artifact pool reference.",
		Long: `PUT /v1/cluster/default-artifact-pool with the supplied pool name.
The server validates that the name resolves to an existing artifact
pool in the cluster (case-insensitive); unknown or non-artifact names
surface as a validation error. Requires admin (cluster:manage);
operators / others receive 403 permission_denied.

The positional accepts a pool NAME only - the cluster_settings row
stores a logical pool concept, not a specific per-node instance.`,
		Args: cobra.ExactArgs(1),
		RunE: runSetDefaultArtifactPool,
	}
	cmd.Flags().StringP(flagOutput, "o", "text", "output format: text|json")
	return cmd
}

func runSetDefaultArtifactPool(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	format, err := outputFormat(cmd, "text")
	if err != nil {
		return err
	}
	name := args[0]
	if name == "" {
		return errors.New("pool name is required")
	}

	got, err := c.SetClusterDefaultArtifactPool(cmd.Context(), name)
	if err != nil {
		return classifyError(err)
	}

	if format == "json" {
		raw, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
		return nil
	}
	printf(cmd, "default artifact pool set to '%s'\n", got.Name)
	return nil
}

func newUnsetDefaultArtifactPoolCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unset-default-artifact-pool",
		Short: "Clear the cluster default artifact pool reference.",
		Long: `DELETE /v1/cluster/default-artifact-pool. Idempotent on server
(clearing an already-unset reference returns 204). Requires admin
(cluster:manage). Non-interactive use without --force is a usage
error - the operation mutates cluster-wide state, so the CLI prompts
when stdin is a TTY.`,
		RunE: runUnsetDefaultArtifactPool,
	}
	cmd.Flags().Bool(flagForce, false, "skip the confirmation prompt")
	return cmd
}

func runUnsetDefaultArtifactPool(cmd *cobra.Command, _ []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	force, _ := cmd.Flags().GetBool(flagForce)

	if !force {
		if !stdinIsTerminal() {
			return errors.New("non-interactive unset requires --force")
		}
		ok, err := confirmYN(cmd, "Clear cluster default artifact pool?")
		if err != nil {
			return err
		}
		if !ok {
			printf(cmd, "aborted\n")
			return nil
		}
	}

	if err := c.ClearClusterDefaultArtifactPool(cmd.Context()); err != nil {
		return classifyError(err)
	}
	printf(cmd, "default artifact pool cleared\n")
	return nil
}
