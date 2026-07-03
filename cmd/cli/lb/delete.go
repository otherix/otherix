// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package lb

import (
	"bufio"
	"errors"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

const flagForce = "force"

// newDeleteCommand returns the `otherix lb delete` cobra command. The
// positional is the load balancer name (the CP DELETE route addresses by
// name). A confirmation prompt fires when stdin is a TTY and --force is
// absent.
func newDeleteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a load balancer by name.",
		Long: `Submits DELETE /v1/loadbalancers/{name}.

--force skips the confirmation prompt; the prompt is offered when stdin
is a TTY and --force is absent (non-TTY automation runs through without
a prompt).

Example:
  otherix lb delete web --force`,
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
	name := args[0]
	if name == "" {
		return errors.New("load balancer name is required")
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
		printf(cmd, "delete load balancer %s? [y/N]: ", name)
		if !readYes() {
			printf(cmd, "aborted\n")
			return nil
		}
	}

	if err := c.DeleteLoadBalancer(cmd.Context(), name); err != nil {
		return classifyError(err)
	}

	switch format {
	case "json":
		printf(cmd, "{\"deleted\":true,\"name\":%q}\n", name)
	default:
		printf(cmd, "load balancer %s deleted\n", name)
	}
	return nil
}

// stdinIsTTY reports whether stdin is a terminal. Tests redirect stdin to
// a pipe / file -> returns false -> confirmation is skipped without
// --force. Parallel of cmd/cli/network.stdinIsTTY.
func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// readYes reads a single line from stdin and returns true on a y/Y
// prefix. Empty input or anything else -> false (the safer side).
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
