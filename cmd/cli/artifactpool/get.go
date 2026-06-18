// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package artifactpool

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

func newGetCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <name|uuid>",
		Short: "Show a cluster-level artifact pool.",
		Long: `Fetches a single artifact pool. The positional accepts a pool name or a
UUID literal; the CP GET-by-id route resolves either one server-side.`,
		Args: cobra.ExactArgs(1),
		RunE: runGet,
	}
	cmd.Flags().StringP(flagOutput, "o", "text", "output format: text|json")
	return cmd
}

func runGet(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	identifier := args[0]
	if identifier == "" {
		return errors.New("artifact-pool identifier is required")
	}
	format, err := outputFormat(cmd, "text")
	if err != nil {
		return err
	}

	ap, err := c.GetArtifactPool(cmd.Context(), identifier)
	if err != nil {
		return classifyError(err)
	}

	if format == "json" {
		return printJSONIndent(cmd, ap)
	}
	return renderGet(cmd, ap)
}

func renderGet(cmd *cobra.Command, ap cpclient.ArtifactPool) error {
	printf(cmd, "id: %s\n", ap.ID)
	printf(cmd, "name: %s\n", ap.Name)
	printf(cmd, "replication_factor: %s\n", rfString(ap.ReplicationFactor))
	printf(cmd, "members: %s\n", membersString(ap))
	printf(cmd, "created_at: %s\n", ap.CreatedAt)
	printf(cmd, "updated_at: %s\n", ap.UpdatedAt)
	return nil
}
