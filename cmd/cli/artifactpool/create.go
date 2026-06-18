// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package artifactpool

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

func newCreateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a cluster-level artifact pool (admin-only).",
		Long: `Submits POST /v1/artifact-pools. Required permission: storage_pool:manage.
An artifact pool is a cluster-level content-addressed store for snapshots (and,
later, images). --replication-factor is the durability target K (an integer >= 1,
or 'all' for every member); not enforced yet in slice B. --members selects which
nodes back it ('all', the default, or a comma-separated node-name list).

Example:
  otherix artifact-pool create artifacts
  otherix artifact-pool create artifacts --replication-factor 2
  otherix artifact-pool create artifacts --replication-factor all
  otherix artifact-pool create artifacts --members node-a,node-b`,
		Args: cobra.ExactArgs(1),
		RunE: runCreate,
	}
	cmd.Flags().String(flagReplicationFactor, "1", "durability target: an integer >= 1 or 'all'")
	cmd.Flags().String(flagMembers, "all", "membership: 'all' or a comma-separated node-name list")
	cmd.Flags().StringP(flagOutput, "o", "text", "output format: text|json")
	return cmd
}

func runCreate(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	name := args[0]
	if name == "" {
		return errors.New("artifact-pool name is required")
	}
	format, err := outputFormat(cmd, "text")
	if err != nil {
		return err
	}

	rfRaw, err := replicationFactorRaw(cmd)
	if err != nil {
		return err
	}
	membership, err := membershipBody(cmd)
	if err != nil {
		return err
	}

	ap, err := c.CreateArtifactPool(cmd.Context(), cpclient.ArtifactPoolCreateBody{
		Name:              name,
		ReplicationFactor: rfRaw,
		Membership:        membership,
	})
	if err != nil {
		return classifyError(err)
	}

	if format == "json" {
		return printJSONIndent(cmd, ap)
	}
	printf(cmd, "artifact-pool %s created (replication_factor=%s, members=%s)\n",
		ap.Name, rfString(ap.ReplicationFactor), membersString(ap))
	return nil
}

// replicationFactorRaw reads --replication-factor and encodes it as the wire
// raw JSON value: the string "all", or an integer (>= 1).
func replicationFactorRaw(cmd *cobra.Command) (json.RawMessage, error) {
	rf, err := cmd.Flags().GetString(flagReplicationFactor)
	if err != nil {
		return nil, err
	}
	if rf == "all" {
		return json.RawMessage(`"all"`), nil
	}
	n, err := strconv.Atoi(rf)
	if err != nil {
		return nil, fmt.Errorf("--%s must be an integer or 'all'", flagReplicationFactor)
	}
	if n < 1 {
		return nil, fmt.Errorf("--%s must be >= 1 or 'all'", flagReplicationFactor)
	}
	return json.RawMessage(strconv.Itoa(n)), nil
}

// membershipBody reads --members and builds the membership object: 'all' (the
// default) maps to {all_nodes: true}; a comma list maps to
// {all_nodes: false, nodes: [...]}.
func membershipBody(cmd *cobra.Command) (any, error) {
	members, err := cmd.Flags().GetString(flagMembers)
	if err != nil {
		return nil, err
	}
	if members == "" || members == "all" {
		return map[string]any{"all_nodes": true}, nil
	}
	parts := strings.Split(members, ",")
	nodes := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		nodes = append(nodes, p)
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("--%s: no node names given", flagMembers)
	}
	return map[string]any{"all_nodes": false, "nodes": nodes}, nil
}
