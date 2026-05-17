// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

const flagForce = "force"

func newDeleteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <vm>",
		Short: "Delete a VM (async).",
		Long: `Submits a vm.delete task. The positional is a VM name (UUID
literals rejected by the server with 400 validation_failed). На
--wait blocks until the agent confirms teardown и the CP soft-delete
chain commits. --force skips the confirmation prompt; otherwise the
CLI prompts when stdin is a TTY.`,
		Args: cobra.ExactArgs(1),
		RunE: runDelete,
	}
	cmd.Flags().Bool(flagWait, false, "block until the task reaches terminal status")
	cmd.Flags().Duration(flagWaitTimeout, defaultWaitTO, "max time to wait when --wait is set")
	cmd.Flags().Bool(flagForce, false, "skip confirmation prompt")
	return cmd
}

func runDelete(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	identifier := args[0]
	wait, err := cmd.Flags().GetBool(flagWait)
	if err != nil {
		return err
	}
	timeout, err := cmd.Flags().GetDuration(flagWaitTimeout)
	if err != nil {
		return err
	}
	force, err := cmd.Flags().GetBool(flagForce)
	if err != nil {
		return err
	}

	if !force && stdinIsTTY() {
		printf(cmd, "delete vm %s? [y/N]: ", identifier)
		if !readYes(cmd) {
			printf(cmd, "aborted\n")
			return nil
		}
	}

	accepted, err := c.DeleteVM(cmd.Context(), identifier)
	if err != nil {
		return classifyError(err)
	}

	printf(cmd, "deleted task=%s status=%s\n", accepted.TaskID, accepted.Status)

	if !wait {
		return nil
	}
	taskID, err := parseTaskID(accepted.TaskID)
	if err != nil {
		return fmt.Errorf("request_failed: %v", err)
	}
	if err := waitForTask(cmd.Context(), cmd, c, taskID, timeout); err != nil {
		return classifyError(err)
	}
	printf(cmd, "vm deleted task=%s\n", accepted.TaskID)
	return nil
}

// stdinIsTTY reports whether stdin is a terminal. Tests redirect
// stdin к a pipe / file → returns false → confirmation is skipped
// без --force. Production interactive use passes through к the
// prompt path.
func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// readYes reads a single line из stdin and returns true on a y/Y
// prefix. Empty input или anything else → false (defaults к no, the
// safer side).
func readYes(cmd *cobra.Command) bool {
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		// EOF на a piped stdin — treat as "no" (already covered by
		// stdinIsTTY in caller, defensive here too).
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
