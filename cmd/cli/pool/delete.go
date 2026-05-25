// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package pool

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

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

  - storage_images materialised in the pool;
  - vm_disks still referencing the pool.

Storage pools have no force-delete counterpart by design — the operator
must remove or migrate the dependent disks/images before retrying. The
failure output lists the blocking resources and their counts so operators
know what to clean up first.

The CLI accepts both a pool name (cluster-wide concept, resolves only
when a single per-node instance exists for that name) and a UUID literal
(targets one specific instance row). Multi-instance pools must be
addressed by UUID; the server returns 400 multiple_instances on a bare
name in that case and the operator should switch to 'otherix pool list'
to discover the per-node UUIDs.

--force skips the confirmation prompt; the prompt is offered when
stdin is a TTY and --force is absent (non-TTY automation runs through
without prompt).

Example:
  otherix pool delete pool-mvp --force
  otherix pool delete <pool-uuid>`,
		Args: cobra.ExactArgs(1),
		RunE: runDelete,
	}
	cmd.Flags().Bool(flagForce, false, "skip the confirmation prompt")
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
	format, err := outputFormat(cmd, "text")
	if err != nil {
		return err
	}

	if !force && stdinIsTTY() {
		printf(cmd, "delete storage pool %s? [y/N]: ", identifier)
		if !readYes() {
			printf(cmd, "aborted\n")
			return nil
		}
	}

	err = c.DeletePool(cmd.Context(), identifier)
	if err != nil {
		var blocked *cpclient.ErrPoolBlocked
		if errors.As(err, &blocked) {
			return renderBlockedDelete(cmd, identifier, blocked, format)
		}
		return classifyError(err)
	}

	switch format {
	case "json":
		printf(cmd, "{\"deleted\":true,\"identifier\":%q}\n", identifier)
	default:
		printf(cmd, "pool %s deleted\n", identifier)
	}
	return nil
}

// renderBlockedDelete prints the blocking-resources envelope in the
// requested format and returns a non-zero-exit error. The text mode
// formats each resource type → count pair so operators can scan the
// list and decide what to clean up first. Mirror template delete's
// rendering — the wire shape is identical so the operator sees
// consistent output across resource types.
func renderBlockedDelete(cmd *cobra.Command, identifier string, blocked *cpclient.ErrPoolBlocked, format string) error {
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
		printf(cmd, "cannot delete pool %s — blocked by:\n", identifier)
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
