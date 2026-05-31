// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// newMemberCommand returns the `otherix cluster member` subcommand group
// with two children: list and remove.
func newMemberCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "member",
		Short: "Inspect and manage etcd cluster membership.",
		Long: `member groups the operator-facing commands for etcd cluster membership.
Use 'list' to inspect HA topology and find dead member ids; use 'remove'
to decommission a dead or replaced replica.

All subcommands require admin (cluster:manage).`,
	}
	cmd.AddCommand(newMemberListCommand())
	cmd.AddCommand(newMemberRemoveCommand())
	return cmd
}

func newMemberListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List etcd cluster members.",
		Long: `Fetches GET /v1/cluster/members and prints each member's id (hex),
name, learner flag, and peer URL(s). Use the hex id with
'otherix cluster member remove <id>' to decommission a dead replica.

Requires admin (cluster:manage); operators and others receive 403
permission_denied.`,
		RunE: runMemberList,
	}
	cmd.Flags().String(flagOutput, "text", "output format: text|json")
	return cmd
}

func runMemberList(cmd *cobra.Command, _ []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	format, err := outputFormat(cmd, "text")
	if err != nil {
		return err
	}

	members, err := c.ListClusterMembers(cmd.Context())
	if err != nil {
		return classifyError(err)
	}

	if format == "json" {
		raw, err := json.MarshalIndent(members, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
		return nil
	}

	if len(members) == 0 {
		printf(cmd, "no cluster members found\n")
		return nil
	}

	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tNAME\tLEARNER\tPEER URL")
	for _, m := range members {
		learner := "no"
		if m.IsLearner {
			learner = "yes"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", m.ID, m.Name, learner, strings.Join(m.PeerURLs, ","))
	}
	_ = tw.Flush()
	return nil
}

func newMemberRemoveCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove <id>",
		Short: "Remove an etcd cluster member by hex id.",
		Long: `Submits DELETE /v1/cluster/members/{id}. The id is the hex member id
as returned by 'otherix cluster member list'. Intended for
decommissioning a dead or replaced replica; etcd rejects a removal
that would lose quorum on a healthy cluster.

Requires admin (cluster:manage); operators and others receive 403
permission_denied.

Non-interactive use without --force is a usage error - removing an
etcd member affects quorum, so the CLI prompts when stdin is a TTY.`,
		Args: cobra.ExactArgs(1),
		RunE: runMemberRemove,
	}
	cmd.Flags().Bool(flagForce, false, "skip the confirmation prompt")
	return cmd
}

func runMemberRemove(cmd *cobra.Command, args []string) error {
	id := args[0]
	if id == "" {
		return errors.New("member id is required")
	}

	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}

	force, _ := cmd.Flags().GetBool(flagForce)
	if !force {
		if !stdinIsTerminal() {
			return errors.New("non-interactive remove requires --force")
		}
		ok, err := confirmYN(cmd, fmt.Sprintf("Remove etcd member %s?", id))
		if err != nil {
			return err
		}
		if !ok {
			printf(cmd, "aborted\n")
			return nil
		}
	}

	if err := c.RemoveClusterMember(cmd.Context(), id); err != nil {
		return classifyError(err)
	}
	printf(cmd, "member %s removed\n", id)
	return nil
}
