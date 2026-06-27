// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package user

import (
	"bufio"
	"errors"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

const flagForce = "force"

// newDeleteCommand returns the `otherix user delete` cobra command. The
// positional <username> is resolved to its UUID client-side before the
// by-id DELETE. A confirmation prompt fires when stdin is a TTY and
// --force is absent.
func newDeleteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <username>",
		Short: "Delete a user by username (admin-only).",
		Long: `Submits DELETE /v1/users/{id}. Required permission: user:manage
(admin role). The username is resolved to its UUID client-side. The
command fails with 409 conflict when the user still owns resources -
transfer or remove those first.

--force skips the confirmation prompt; the prompt is offered only when
stdin is a TTY and --force is absent.`,
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
	username := args[0]
	if username == "" {
		return errors.New("username is required")
	}
	force, err := cmd.Flags().GetBool(flagForce)
	if err != nil {
		return err
	}
	format, err := outputFormat(cmd, "text")
	if err != nil {
		return err
	}

	u, err := resolveUserID(cmd.Context(), c, username)
	if err != nil {
		return err
	}

	if !force && stdinIsTTY() {
		printf(cmd, "delete user %s? [y/N]: ", username)
		if !readYes() {
			printf(cmd, "aborted\n")
			return nil
		}
	}

	if err := c.DeleteUser(cmd.Context(), u.ID); err != nil {
		return classifyError(err)
	}

	switch format {
	case "json":
		printf(cmd, "{\"deleted\":true,\"username\":%q}\n", username)
	default:
		printf(cmd, "user %s deleted\n", username)
	}
	return nil
}

// stdinIsTTY reports whether stdin is a terminal. Tests redirect stdin
// to a pipe / file -> returns false -> confirmation is skipped without
// --force.
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
