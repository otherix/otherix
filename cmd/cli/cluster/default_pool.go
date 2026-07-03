// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cluster

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

func newGetDefaultPoolCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get-default-pool",
		Short: "Show the cluster default pool reference.",
		Long: `Fetches /v1/cluster/default-pool. When unset, prints an actionable
informational line to stdout and exits 0 (parseable by tooling that
distinguishes set/unset without inspecting exit codes).`,
		RunE: runGetDefaultPool,
	}
	cmd.Flags().StringP(flagOutput, "o", "text", "output format: text|json")
	return cmd
}

func runGetDefaultPool(cmd *cobra.Command, _ []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	format, err := outputFormat(cmd, "text")
	if err != nil {
		return err
	}

	got, err := c.GetClusterDefaultPool(cmd.Context())
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
		printf(cmd, "no default pool configured (run 'otherix cluster set-default-pool <name>' to configure)\n")
		return nil
	}
	printf(cmd, "default-pool: %s\n", got.Name)
	return nil
}

func newSetDefaultPoolCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set-default-pool <name>",
		Short: "Set the cluster default pool reference.",
		Long: `PUT /v1/cluster/default-pool with the supplied pool name. The server
validates that at least one pool instance with this name exists in the
cluster (case-insensitive); unknown names surface as 400
pool_not_found. Requires admin (cluster:manage); operators / others
receive 403 permission_denied.

The positional accepts a pool NAME only — the cluster_settings row
stores a logical pool concept, not a specific per-node instance.
Pass a UUID literal here and the server returns 400 pool_not_found.`,
		Args: cobra.ExactArgs(1),
		RunE: runSetDefaultPool,
	}
	cmd.Flags().StringP(flagOutput, "o", "text", "output format: text|json")
	return cmd
}

func runSetDefaultPool(cmd *cobra.Command, args []string) error {
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

	got, err := c.SetClusterDefaultPool(cmd.Context(), name)
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
	printf(cmd, "default pool set to '%s'\n", got.Name)
	return nil
}

func newUnsetDefaultPoolCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unset-default-pool",
		Short: "Clear the cluster default pool reference.",
		Long: `DELETE /v1/cluster/default-pool. Idempotent on server (clearing an
already-unset reference returns 204). Requires admin (cluster:manage).
Non-interactive use without --force is a usage error — the operation
mutates cluster-wide state, so the CLI prompts when stdin is a TTY.`,
		RunE: runUnsetDefaultPool,
	}
	cmd.Flags().Bool(flagForce, false, "skip the confirmation prompt")
	return cmd
}

func runUnsetDefaultPool(cmd *cobra.Command, _ []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	force, _ := cmd.Flags().GetBool(flagForce)

	if !force {
		if !stdinIsTerminal() {
			return errors.New("non-interactive unset requires --force")
		}
		ok, err := confirmYN(cmd, "Clear cluster default pool?")
		if err != nil {
			return err
		}
		if !ok {
			printf(cmd, "aborted\n")
			return nil
		}
	}

	if err := c.ClearClusterDefaultPool(cmd.Context()); err != nil {
		return classifyError(err)
	}
	printf(cmd, "default pool cleared\n")
	return nil
}
