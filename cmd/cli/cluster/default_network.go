// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cluster

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

func newGetDefaultNetworkCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get-default-network",
		Short: "Show the cluster default network reference.",
		Long: `Fetches /v1/cluster/default-network. When unset, prints an actionable
informational line to stdout and exits 0 (parseable by tooling that
distinguishes set/unset without inspecting exit codes).`,
		RunE: runGetDefaultNetwork,
	}
	cmd.Flags().StringP(flagOutput, "o", "text", "output format: text|json")
	return cmd
}

func runGetDefaultNetwork(cmd *cobra.Command, _ []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	format, err := outputFormat(cmd, "text")
	if err != nil {
		return err
	}

	got, err := c.GetClusterDefaultNetwork(cmd.Context())
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
		printf(cmd, "no default network configured (run 'otherix cluster set-default-network <name>' to configure)\n")
		return nil
	}
	printf(cmd, "default-network: %s\n", got.Name)
	return nil
}

func newSetDefaultNetworkCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set-default-network <name>",
		Short: "Set the cluster default network reference.",
		Long: `PUT /v1/cluster/default-network with the supplied network name. The
server validates that a bridge network with this name exists; unknown
or non-bridge names surface as 400 validation_failed. Requires admin
(cluster:manage); operators / others receive 403 permission_denied.

When set, 'otherix vm create' without --network attaches one NIC to
this network.`,
		Args: cobra.ExactArgs(1),
		RunE: runSetDefaultNetwork,
	}
	cmd.Flags().StringP(flagOutput, "o", "text", "output format: text|json")
	return cmd
}

func runSetDefaultNetwork(cmd *cobra.Command, args []string) error {
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
		return errors.New("network name is required")
	}

	got, err := c.SetClusterDefaultNetwork(cmd.Context(), name)
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
	printf(cmd, "default network set to '%s'\n", got.Name)
	return nil
}

func newUnsetDefaultNetworkCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unset-default-network",
		Short: "Clear the cluster default network reference.",
		Long: `DELETE /v1/cluster/default-network. Idempotent on server (clearing an
already-unset reference returns 204). Requires admin (cluster:manage).
Non-interactive use without --force is a usage error - the operation
mutates cluster-wide state, so the CLI prompts when stdin is a TTY.`,
		RunE: runUnsetDefaultNetwork,
	}
	cmd.Flags().Bool(flagForce, false, "skip the confirmation prompt")
	return cmd
}

func runUnsetDefaultNetwork(cmd *cobra.Command, _ []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	force, _ := cmd.Flags().GetBool(flagForce)

	if !force {
		if !stdinIsTerminal() {
			return errors.New("non-interactive unset requires --force")
		}
		ok, err := confirmYN(cmd, "Clear cluster default network?")
		if err != nil {
			return err
		}
		if !ok {
			printf(cmd, "aborted\n")
			return nil
		}
	}

	if err := c.ClearClusterDefaultNetwork(cmd.Context()); err != nil {
		return classifyError(err)
	}
	printf(cmd, "default network cleared\n")
	return nil
}
