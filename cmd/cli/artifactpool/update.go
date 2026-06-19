// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package artifactpool

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

func newUpdateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update <name|uuid>",
		Short: "Update an artifact pool's replication factor and/or membership (admin-only).",
		Long: `Submits PATCH /v1/artifact-pools/{id}. Required permission: storage_pool:manage.
The positional accepts a pool name or a UUID literal; the CP route resolves
either one server-side. The pool name is immutable - only --replication-factor
and --members may change. At least one of the two must be supplied; an omitted
flag is left untouched.

--replication-factor is the durability target K (an integer >= 1, or 'all' for
every member). Increasing it drives immediate re-replication: the cluster
reconciles toward the new factor. Decreasing it leaves the extra copies in
place until they are garbage-collected, so durability is never reduced below
the new factor at any instant.

--members selects which nodes back the pool ('all' or a comma-separated
node-name list).

Example:
  otherix artifact-pool update artifacts --replication-factor 3
  otherix artifact-pool update artifacts --replication-factor all
  otherix artifact-pool update artifacts --members node-a,node-b
  otherix artifact-pool update artifacts --replication-factor 2 --members all`,
		Args: cobra.ExactArgs(1),
		RunE: runUpdate,
	}
	cmd.Flags().String(flagReplicationFactor, "", "durability target: an integer >= 1 or 'all'")
	cmd.Flags().String(flagMembers, "", "membership: 'all' or a comma-separated node-name list")
	cmd.Flags().StringP(flagOutput, "o", "text", "output format: text|json")
	return cmd
}

func runUpdate(cmd *cobra.Command, args []string) error {
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

	rfChanged := cmd.Flags().Changed(flagReplicationFactor)
	membersChanged := cmd.Flags().Changed(flagMembers)
	if !rfChanged && !membersChanged {
		return errors.New("specify --replication-factor and/or --members")
	}

	var body cpclient.ArtifactPoolUpdateBody
	if rfChanged {
		rf, err := replicationFactorRaw(cmd)
		if err != nil {
			return err
		}
		body.ReplicationFactor = &rf
	}
	if membersChanged {
		membership, err := membershipBody(cmd)
		if err != nil {
			return err
		}
		body.Membership = membership
	}

	ap, err := c.UpdateArtifactPool(cmd.Context(), identifier, body)
	if err != nil {
		return classifyError(err)
	}

	if format == "json" {
		return printJSONIndent(cmd, ap)
	}
	return renderGet(cmd, ap)
}
