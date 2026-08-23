// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
	"github.com/otherix/otherix/cmd/cli/internal/manifest"
)

const (
	flagListPool   = "pool"
	flagListNode   = "node"
	flagListStatus = "status"
	flagListLimit  = "limit"
	flagListCursor = "cursor"
	flagShowIDs    = "show-ids"

	defaultListLimit = 20
)

func newListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List VMs visible to the caller.",
		Long: `Cursor-paginated list. Server-side filters: pool (name or uuid),
node (name or uuid — filters by the agent-reported current location,
not by pinned intent), status (creating/running/migrating/paused/stopped/
error/gone/orphaned). The next page's cursor lives in the JSON envelope's
meta.next_cursor and is opaque — re-pass with --cursor.

A VM reads orphaned when the node holding it was force-deleted: the guest
and its disk stayed on that host, so nothing here can start, move, or reach
them. Deleting the record is the only exit, and it tears the guest down if
that host ever rejoins the cluster.`,
		RunE: runList,
	}

	cmd.Flags().String(flagListPool, "", "filter by storage pool name or uuid")
	cmd.Flags().String(flagListNode, "", "filter by current-location node name or uuid")
	cmd.Flags().String(flagListStatus, "", "filter by status")
	cmd.Flags().Int(flagListLimit, defaultListLimit, "page size (1..200)")
	cmd.Flags().String(flagListCursor, "", "opaque cursor from a previous page")
	cmd.Flags().StringP(flagOutput, "o", "table", "output format: table|json|yaml")
	cmd.Flags().Bool(flagShowIDs, false, "include VM UUIDs in the table output")

	return cmd
}

func runList(cmd *cobra.Command, _ []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}

	pool, err := cmd.Flags().GetString(flagListPool)
	if err != nil {
		return err
	}
	node, err := cmd.Flags().GetString(flagListNode)
	if err != nil {
		return err
	}
	status, err := cmd.Flags().GetString(flagListStatus)
	if err != nil {
		return err
	}
	limit, err := cmd.Flags().GetInt(flagListLimit)
	if err != nil {
		return err
	}
	cursor, err := cmd.Flags().GetString(flagListCursor)
	if err != nil {
		return err
	}
	format, err := outputFormat(cmd, "table", "yaml")
	if err != nil {
		return err
	}
	showIDs, err := cmd.Flags().GetBool(flagShowIDs)
	if err != nil {
		return err
	}

	vms, err := c.ListVMs(cmd.Context(), cpclient.ListVMsParams{
		Limit:  limit,
		Cursor: cursor,
		Pool:   pool,
		Node:   node,
		Status: status,
	})
	if err != nil {
		return classifyError(err)
	}

	if format == "yaml" {
		docs := make([][]byte, 0, len(vms.Data))
		for _, vm := range vms.Data {
			out, err := manifest.ProjectVM(vm)
			if err != nil {
				return err
			}
			docs = append(docs, out)
		}
		printf(cmd, "%s", manifest.JoinDocuments(docs))
		return nil
	}

	switch format {
	case "json":
		raw, err := json.MarshalIndent(vms, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
	default:
		printVMTable(cmd, vms, showIDs)
	}
	return nil
}

// printVMTable renders an aligned table to stdout. text/tabwriter
// computes column widths once and flushes on Close. The bottom hint
// includes meta.next_cursor when populated so operators can chain
// the next page without digging in JSON. The ID column is hidden by
// default — referenced-resource fields are names, and the VM's own
// UUID is operator-noise unless --show-ids opts back in. Columns:
// NAME STATUS ARCH NODE POOL NETWORK AGE — placement (ARCH/NODE) and
// connectivity (NETWORK) are the operator-relevant axes; AGE is the
// compact created_at delta (matching node/pool list). The image source
// URL lives in `vm get` / `--output json`, not the list.
func printVMTable(cmd *cobra.Command, vms cpclient.VMList, showIDs bool) {
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	if showIDs {
		_, _ = fmt.Fprintln(tw, "ID\tNAME\tSTATUS\tARCH\tNODE\tPOOL\tNETWORK\tAGE")
	} else {
		_, _ = fmt.Fprintln(tw, "NAME\tSTATUS\tARCH\tNODE\tPOOL\tNETWORK\tAGE")
	}
	for _, vm := range vms.Data {
		node := dashIfEmpty(deref(vm.Node))
		network := renderVMNetwork(vm.Networks)
		age := humanAge(vm.CreatedAt)
		if showIDs {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				vm.ID, vm.Name, vm.Status.Phase, vm.Architecture, node, vm.Pool, network, age)
		} else {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				vm.Name, vm.Status.Phase, vm.Architecture, node, vm.Pool, network, age)
		}
	}
	_ = tw.Flush()
	printOrphanHint(cmd, vms)
	if vms.Meta.NextCursor != nil {
		printNextCursor(cmd, *vms.Meta.NextCursor)
	}
}

// maxOrphanHintNames bounds how many VM names the orphan hint spells out before
// it falls back to a counter. A force-deleted node orphans every VM it held, and
// a forty-name footnote buries the sentence it is attached to.
const maxOrphanHintNames = 3

// printOrphanHint explains the `orphaned` status below the table when the page
// carries one, and prints nothing otherwise.
//
// The bare word in the STATUS column answers none of the three questions an
// operator has. What happened: the node holding the VM was force-deleted (the
// only producer of this phase). Where the data went: pools are node-local, so
// the guest and its disk stayed on that host and nothing here can start, move,
// or reach them. And what the exit costs: deleting the record is the only one
// there is, but a returning host that reports a deleted VM is answered with a
// teardown signal - so the guest dies when its hardware comes back. That last
// point is a real decision, not a formality, and the operator cannot weigh it
// from a nine-letter status.
func printOrphanHint(cmd *cobra.Command, vms cpclient.VMList) {
	var names []string
	for _, vm := range vms.Data {
		if vm.Status.Phase == "orphaned" {
			names = append(names, vm.Name)
		}
	}
	if len(names) == 0 {
		return
	}

	// Singular and plural are spelled out rather than assembled from parts:
	// every clause below carries number, and threading six agreement points
	// through one format string reads worse than two paragraphs.
	if len(names) == 1 {
		printf(cmd, "\n1 VM is orphaned (%s): its node was force-deleted, so the guest and\n"+
			"its disk stayed on that host and cannot be started, moved, or reached from\n"+
			"here. Deleting the record is the only exit - and it tears the guest down if\n"+
			"that host ever rejoins the cluster:\n"+
			"  %s delete %s\n", names[0], vmCommandPath(cmd), names[0])
		return
	}
	printf(cmd, "\n%d VMs are orphaned (%s): their nodes were\n"+
		"force-deleted, so the guests and their disks stayed on those hosts and cannot\n"+
		"be started, moved, or reached from here. Deleting a record is the only exit -\n"+
		"and it tears that guest down if its host ever rejoins the cluster:\n"+
		"  %s delete <name>\n", len(names), renderOrphanNames(names), vmCommandPath(cmd))
}

// renderOrphanNames lists up to maxOrphanHintNames names, summarising the rest
// as a counter.
func renderOrphanNames(names []string) string {
	if len(names) <= maxOrphanHintNames {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more",
		strings.Join(names[:maxOrphanHintNames], ", "), len(names)-maxOrphanHintNames)
}

// vmCommandPath returns the path of the `vm` command group, so a hint can name
// a sibling subcommand under whatever binary name the caller invoked. Falls back
// to the shipped name for a parentless command (the table renderer is unit-tested
// against a bare cobra.Command).
func vmCommandPath(cmd *cobra.Command) string {
	if cmd.Parent() != nil {
		return cmd.Parent().CommandPath()
	}
	return "otherix vm"
}

// renderVMNetwork formats the NETWORK column from the VM's ordered
// network names: a single network renders its name; multiple render the
// primary name plus a "(+N)" counter of the remaining NICs. A VM with no
// NIC renders the "-" placeholder.
func renderVMNetwork(networks []string) string {
	switch len(networks) {
	case 0:
		return "-"
	case 1:
		return networks[0]
	default:
		return fmt.Sprintf("%s (+%d)", networks[0], len(networks)-1)
	}
}

// deref returns the pointee of p, or the empty string when p is nil.
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// dashIfEmpty renders the "-" placeholder for an empty cell.
func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
