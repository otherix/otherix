// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package pool

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

const flagForce = "force"

// newDeleteCommand returns the `otherix pool delete` cobra command.
// Positional accepts a pool name or a UUID literal; the server resolves
// either form. Confirmation prompt fires when stdin is a TTY and --force
// is absent, mirror VM delete UX.
func newDeleteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <identifier>",
		Short: "Delete a storage pool by name or UUID (admin-only).",
		Long: `Submits DELETE /v1/storage-pools/{identifier}. The command fails with
409 conflict when the pool still has dependent resources:

  - vm_disks still referencing the pool.

Storage pools have no force-delete counterpart by design — the operator
must remove or migrate the dependent disks before retrying. The failure
output lists the blocking resource counts and, when the blocker is
vm_disks, the names of the VMs holding a disk on the pool so operators
know exactly what to clean up first. (The agent-owned image cache is not
a delete blocker.)

A pool name is a cluster-wide concept that can map to one per-node
instance row on each node. The CLI accepts:

  - a UUID literal — targets one specific instance row directly;
  - a name that resolves to a single instance — deleted as-is;
  - a name + --node <node> — deletes the instance on that node;
  - a bare name that maps to multiple instances — the command lists the
    per-node instances (node + UUID) and the two ways to retry.

--force skips the confirmation prompt; the prompt is offered when
stdin is a TTY and --force is absent (non-TTY automation runs through
without prompt).

Example:
  otherix pool delete pool-mvp --force
  otherix pool delete default --node node-1
  otherix pool delete <pool-uuid>`,
		Args: cobra.ExactArgs(1),
		RunE: runDelete,
	}
	cmd.Flags().Bool(flagForce, false, "skip the confirmation prompt")
	cmd.Flags().String(flagNode, "", "delete the instance on this node (disambiguates a multi-instance pool name)")
	cmd.Flags().String(flagOutput, "text", "output format: text|json")
	return cmd
}

func runDelete(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	identifier := args[0]
	if identifier == "" {
		return errors.New("pool identifier is required")
	}
	force, err := cmd.Flags().GetBool(flagForce)
	if err != nil {
		return err
	}
	node, err := cmd.Flags().GetString(flagNode)
	if err != nil {
		return err
	}
	format, err := outputFormat(cmd, "text")
	if err != nil {
		return err
	}

	// target is what we send to the server (a UUID once --node resolves an
	// instance, else the raw identifier); label is what the operator sees.
	target, label, err := resolvePoolDeleteTarget(cmd.Context(), c, identifier, node)
	if err != nil {
		return err
	}

	if !force && stdinIsTTY() {
		printf(cmd, "delete storage pool %s? [y/N]: ", label)
		if !readYes() {
			printf(cmd, "aborted\n")
			return nil
		}
	}

	err = c.DeletePool(cmd.Context(), target)
	if err != nil {
		var blocked *cpclient.ErrPoolBlocked
		if errors.As(err, &blocked) {
			return renderBlockedDelete(cmd, label, blocked, blockingPoolVMs(cmd.Context(), c, target), format)
		}
		var apiErr *cpclient.APIError
		if errors.As(err, &apiErr) && apiErr.Code == "pool_name_ambiguous" {
			return renderAmbiguousDelete(cmd, c, identifier, format)
		}
		return classifyError(err)
	}

	switch format {
	case "json":
		printf(cmd, "{\"deleted\":true,\"identifier\":%q}\n", label)
	default:
		printf(cmd, "pool %s deleted\n", label)
	}
	return nil
}

// resolvePoolDeleteTarget turns a (identifier, --node) pair into the
// instance to delete. With --node empty it passes the identifier through
// unchanged (a UUID, or a name the server resolves when it maps to a
// single instance). With --node set it rejects a UUID identifier (which
// already targets one row), fetches the pool's per-node instances, and
// returns the UUID of the instance on that node. The returned label is
// the operator-facing description used in prompts and messages.
func resolvePoolDeleteTarget(ctx context.Context, c *cpclient.Client, identifier, node string) (target, label string, err error) {
	if node == "" {
		return identifier, identifier, nil
	}
	if _, perr := uuid.Parse(identifier); perr == nil {
		return "", "", fmt.Errorf("--%s cannot be combined with a UUID identifier (a UUID already targets one instance)", flagNode)
	}
	concept, _, gerr := c.GetPoolByName(ctx, identifier)
	if gerr != nil {
		return "", "", classifyError(gerr)
	}
	inst, ok := findPoolInstanceOnNode(concept.Instances, node)
	if !ok {
		return "", "", fmt.Errorf("pool %q has no instance on node %q; instances exist on: %s",
			identifier, node, strings.Join(poolInstanceNodes(concept.Instances), ", "))
	}
	return inst.ID.String(), fmt.Sprintf("%s on node %s", identifier, node), nil
}

// findPoolInstanceOnNode returns the instance whose owning node matches
// node. The (node_id, lower(name)) uniqueness guarantees at most one.
func findPoolInstanceOnNode(insts []cpclient.Pool, node string) (cpclient.Pool, bool) {
	for _, inst := range insts {
		if inst.Node == node {
			return inst, true
		}
	}
	return cpclient.Pool{}, false
}

// poolInstanceNodes lists the owning-node names of a pool's instances,
// for the "no instance on node X; instances exist on: ..." hint.
func poolInstanceNodes(insts []cpclient.Pool) []string {
	nodes := make([]string, 0, len(insts))
	for _, inst := range insts {
		nodes = append(nodes, inst.Node)
	}
	return nodes
}

// blockingPoolVMs returns the names of VMs holding a disk on the pool,
// fetched via the vm list pool filter. Best-effort: a lookup failure
// yields nil so the blocked-delete output degrades to the bare counts
// rather than masking the original 409.
func blockingPoolVMs(ctx context.Context, c *cpclient.Client, poolIdentifier string) []string {
	list, err := c.ListVMs(ctx, cpclient.ListVMsParams{Pool: poolIdentifier, Limit: 200})
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(list.Data))
	for _, vm := range list.Data {
		names = append(names, vm.Name)
	}
	return names
}

// renderAmbiguousDelete is the actionable form of the server's
// pool_name_ambiguous error: it fetches the pool's per-node instances and
// prints each one's node + UUID plus the two ways to retry (by node or by
// UUID), then returns a non-zero-exit error. A failed disambiguation
// lookup falls back to the raw server error.
func renderAmbiguousDelete(cmd *cobra.Command, c *cpclient.Client, name, format string) error {
	concept, _, err := c.GetPoolByName(cmd.Context(), name)
	if err != nil {
		return classifyError(err)
	}
	insts := concept.Instances
	if format == "json" {
		rows := make([]map[string]string, 0, len(insts))
		for _, inst := range insts {
			rows = append(rows, map[string]string{"node": inst.Node, "id": inst.ID.String()})
		}
		raw, merr := json.MarshalIndent(map[string]any{
			"deleted": false, "identifier": name,
			"code": "pool_name_ambiguous", "instances": rows,
		}, "", "  ")
		if merr != nil {
			return fmt.Errorf("marshal json: %v", merr)
		}
		printf(cmd, "%s\n", raw)
		return fmt.Errorf("pool %q is ambiguous: %d instances", name, len(insts))
	}
	printf(cmd, "pool %q maps to multiple per-node instances — pick one:\n\n", name)
	for _, inst := range insts {
		printf(cmd, "  node %-14s %s\n", inst.Node, inst.ID)
	}
	printf(cmd, "\nretry by node:\n  otherix pool delete %s --node <node>\n", name)
	printf(cmd, "or by instance UUID:\n  otherix pool delete <uuid>\n")
	return fmt.Errorf("pool %q is ambiguous: %d instances", name, len(insts))
}

// renderBlockedDelete prints the blocking-resources envelope in the
// requested format and returns a non-zero-exit error. The text mode
// formats each resource type → count pair so operators can scan the
// list and decide what to clean up first, and lists the names of the VMs
// holding a disk on the pool (vms, when resolved) so the operator knows
// exactly which VMs to remove or migrate. Mirrors the network delete
// rendering — the wire shape is identical so the operator sees
// consistent output across resource types.
func renderBlockedDelete(cmd *cobra.Command, identifier string, blocked *cpclient.ErrPoolBlocked, vms []string, format string) error {
	switch format {
	case "json":
		payload := map[string]any{
			"deleted":            false,
			"identifier":         identifier,
			"code":               blocked.Code,
			"blocking_resources": blocked.Resources,
		}
		if len(vms) > 0 {
			payload["vms"] = vms
		}
		raw, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
	default:
		printf(cmd, "cannot delete pool %s — blocked by:\n", identifier)
		keys := make([]string, 0, len(blocked.Resources))
		for k := range blocked.Resources {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			printf(cmd, "  - %s: %d\n", k, blocked.Resources[k])
		}
		if len(vms) > 0 {
			printf(cmd, "\nVMs with a disk on this pool:\n")
			for _, name := range vms {
				printf(cmd, "  - %s\n", name)
			}
		}
		printf(cmd, "\nclean up the listed dependents before retrying delete.\n")
	}
	return fmt.Errorf("pool %s blocked: %s", identifier, blocked.Code)
}

// stdinIsTTY reports whether stdin is a terminal. Tests redirect stdin
// to a pipe / file → returns false → confirmation is skipped without
// --force. Production interactive use passes through to the prompt path.
// Parallel of cmd/cli/vm.stdinIsTTY; kept package-local pending
// promotion when a third group needs it.
func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// readYes reads a single line from stdin and returns true on a y/Y
// prefix. Empty input or anything else → false (defaults to no, the
// safer side).
func readYes() bool {
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		return false
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	switch line[0] {
	case 'y', 'Y':
		return true
	}
	return false
}
