// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package artifactpool

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

func newDeleteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <name|uuid>",
		Short: "Delete an artifact pool by name or UUID (admin-only).",
		Long: `Submits DELETE /v1/artifact-pools/{id}. Required permission:
storage_pool:manage (admin role). The command fails with 409 conflict when
the pool still backs snapshots; artifact pools have no force-delete
counterpart by design - the operator must delete the dependent snapshots
first. The failure output lists the blocking snapshots count so operators
know what to clean up.

The positional accepts a pool name or a UUID literal; the CP DELETE route
resolves either one server-side.

--force skips the confirmation prompt; the prompt is offered when stdin is a
TTY and --force is absent (non-TTY automation runs through without prompt).

Example:
  otherix artifact-pool delete artifacts --force
  otherix artifact-pool delete <pool-uuid>`,
		Args: cobra.ExactArgs(1),
		RunE: runDelete,
	}
	cmd.Flags().Bool(flagForce, false, "skip the confirmation prompt")
	cmd.Flags().StringP(flagOutput, "o", "text", "output format: text|json")
	return cmd
}

func runDelete(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	identifier := args[0]
	if identifier == "" {
		return errors.New("artifact-pool identifier is required")
	}
	force, err := cmd.Flags().GetBool(flagForce)
	if err != nil {
		return err
	}
	format, err := outputFormat(cmd, "text")
	if err != nil {
		return err
	}

	if !force && stdinIsTTY() {
		printf(cmd, "delete artifact-pool %s? [y/N]: ", identifier)
		if !readYes() {
			printf(cmd, "aborted\n")
			return nil
		}
	}

	err = c.DeleteArtifactPool(cmd.Context(), identifier)
	if err != nil {
		var blocked *cpclient.ErrArtifactPoolBlocked
		if errors.As(err, &blocked) {
			return renderBlockedDelete(cmd, identifier, blocked, format)
		}
		return classifyError(err)
	}

	switch format {
	case "json":
		printf(cmd, "{\"deleted\":true,\"identifier\":%q}\n", identifier)
	default:
		printf(cmd, "artifact-pool %s deleted\n", identifier)
	}
	return nil
}

// renderBlockedDelete prints the blocking-resources envelope in the requested
// format and returns a non-zero-exit error. Mirrors network delete's
// rendering - the wire shape is identical so operators see consistent output
// across resource types.
func renderBlockedDelete(cmd *cobra.Command, identifier string, blocked *cpclient.ErrArtifactPoolBlocked, format string) error {
	switch format {
	case "json":
		payload := map[string]any{
			"deleted":            false,
			"identifier":         identifier,
			"code":               blocked.Code,
			"blocking_resources": blocked.Resources,
		}
		raw, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
	default:
		printf(cmd, "cannot delete artifact-pool %s - blocked by:\n", identifier)
		keys := make([]string, 0, len(blocked.Resources))
		for k := range blocked.Resources {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			printf(cmd, "  - %s: %d\n", k, blocked.Resources[k])
		}
		printf(cmd, "\nclean up the listed dependents before retrying delete.\n")
	}
	return fmt.Errorf("artifact-pool %s blocked: %s", identifier, blocked.Code)
}
