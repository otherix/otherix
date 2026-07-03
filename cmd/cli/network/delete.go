// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package network

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

// newDeleteCommand returns the `otherix network delete` cobra command.
// The positional accepts a network name or a UUID literal; a name is
// resolved to its UUID client-side. Confirmation prompt fires when
// stdin is a TTY and --force is absent, mirror pool delete UX.
func newDeleteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <name|uuid>",
		Short: "Delete a network by name or UUID (admin-only).",
		Long: `Submits DELETE /v1/networks/{id}. Required permission:
network:manage (admin role). The command fails with 409 conflict when
the network still has dependent vm_nics; networks have no force-delete
counterpart by design - the operator must remove or migrate the
dependent VMs first. The failure output lists the blocking vm_nics
count so operators know what to clean up.

The positional accepts a network name or a UUID literal. The CP DELETE
route accepts only a UUID, so a name is resolved to its UUID
client-side (the name is globally unique).

--force skips the confirmation prompt; the prompt is offered when
stdin is a TTY and --force is absent (non-TTY automation runs through
without prompt).

Example:
  otherix network delete net-dev --force
  otherix network delete <network-uuid>`,
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
		return errors.New("network identifier is required")
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
		printf(cmd, "delete network %s? [y/N]: ", identifier)
		if !readYes() {
			printf(cmd, "aborted\n")
			return nil
		}
	}

	err = c.DeleteNetwork(cmd.Context(), identifier)
	if err != nil {
		var blocked *cpclient.ErrNetworkBlocked
		if errors.As(err, &blocked) {
			return renderBlockedDelete(cmd, identifier, blocked, format)
		}
		return classifyError(err)
	}

	switch format {
	case "json":
		printf(cmd, "{\"deleted\":true,\"identifier\":%q}\n", identifier)
	default:
		printf(cmd, "network %s deleted\n", identifier)
	}
	return nil
}

// renderBlockedDelete prints the blocking-resources envelope in the
// requested format and returns a non-zero-exit error. Mirror pool
// delete's rendering - the wire shape is identical so operators see
// consistent output across resource types.
func renderBlockedDelete(cmd *cobra.Command, identifier string, blocked *cpclient.ErrNetworkBlocked, format string) error {
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
		printf(cmd, "cannot delete network %s - blocked by:\n", identifier)
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
	return fmt.Errorf("network %s blocked: %s", identifier, blocked.Code)
}

// stdinIsTTY reports whether stdin is a terminal. Tests redirect stdin
// to a pipe / file → returns false → confirmation is skipped without
// --force. Parallel of cmd/cli/pool.stdinIsTTY.
func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// readYes reads a single line from stdin and returns true on a y/Y
// prefix. Empty input or anything else → false (the safer side).
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
