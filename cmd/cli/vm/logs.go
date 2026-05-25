// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

// newLogsCommand returns `otherix vm logs <vm-name>`. The streaming
// session writes the agent's text/plain bytes to stdout verbatim;
// errors before the first byte surface through classifyError; errors
// mid-stream end the copy without a special status (mirrors kubectl
// logs behaviour).
//
// Flag values live in a closure rather than at package scope so the
// race detector stays happy when parallel tests build their own
// command trees via NewCommand.
func newLogsCommand() *cobra.Command {
	var (
		tail   int
		follow bool
	)
	cmd := &cobra.Command{
		Use:   "logs <vm-name>",
		Short: "Stream a VM's serial console output (kubectl-style).",
		Long: `Stream a VM's serial console output to stdout.

By default, the full retained history is shown. Use --tail to limit
the trailing lines. Use --follow to keep streaming live bytes after
the initial history flushes.

Concurrent with 'otherix vm console' - logs do not contend for the
single console-session slot enforced by the agent.

Examples:
  otherix vm logs demo
  otherix vm logs demo --tail=100
  otherix vm logs demo --follow
  otherix vm logs demo --tail=50 --follow
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogs(cmd, args, tail, follow)
		},
	}
	cmd.Flags().IntVar(&tail, "tail", -1,
		"Number of trailing lines from history (-1 = all, 0 = none)")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false,
		"Continue streaming live output after the initial history")
	return cmd
}

func runLogs(cmd *cobra.Command, args []string, tail int, follow bool) error {
	vmName := args[0]
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	reader, err := c.VMLogs(cmd.Context(), vmName, tail, follow)
	if err != nil {
		return classifyError(err)
	}
	defer func() { _ = reader.Close() }()

	if _, err := io.Copy(os.Stdout, reader); err != nil {
		// Operator-triggered Ctrl+C arrives as a cancelled context;
		// surface that as a clean exit rather than a stack trace.
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return fmt.Errorf("stream: %v", err)
	}
	return nil
}
