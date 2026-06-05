// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	"github.com/otherix/otherix/cmd/cli/internal/clierr"
	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// clientFromFlags resolves --endpoint / --token / --cluster /
// --config + their env-var counterparts through cliauth.BuildClient
// and returns a ready *cpclient.Client. Errors are already operator-
// shaped (see cliauth.translateResolveError); cobra's RunE chain
// surfaces them through main()'s "error: <msg>" stderr formatter.
func clientFromFlags(cmd *cobra.Command) (*cpclient.Client, error) {
	return cliauth.BuildClient(cmd)
}

// printf writes to cmd.OutOrStdout — preserves cobra's testability
// (each command's stdout / stderr is rebindable in tests).
func printf(cmd *cobra.Command, format string, args ...any) {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), format, args...)
}

// printJSON writes raw server JSON to the command's stdout, re-indented two
// spaces. Used by `--output json` so the CLI echoes exactly what the CP
// returned (absent-vs-null is the server's choice), never a lossy re-marshal
// of a decoded struct.
func printJSON(cmd *cobra.Command, raw json.RawMessage) error {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return fmt.Errorf("indent json: %v", err)
	}
	printf(cmd, "%s\n", buf.String())
	return nil
}

// classifyError maps an err from cpclient to the operator-facing
// "<category>: <detail>" prefix shape that ping-agent established
// (see cmd/cli/ping_agent.go). Shell scripts grep on the prefix —
// this string is part of the CLI's contract. Returns an error so
// callers can simply `return classifyError(err)` from RunE; main
// renders it to stderr with the standard "error: " preamble.
func classifyError(err error) error {
	return clierr.Classify(err)
}

// parseTaskID parses the task id from the CP's AsyncTaskAccepted
// envelope. Surfaces a helpful error if the CP ever returns a
// malformed value (defensive — should not happen in practice).
func parseTaskID(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, fmt.Errorf("malformed task id %q: %v", raw, err)
	}
	return id, nil
}

// waitForTask polls the supplied taskID until it reaches a terminal
// state, the timeout fires, or ctx is cancelled. The progress
// callback emits a single dot per poll to cmd.ErrOrStderr — picks
// stderr so stdout stays parseable for tooling that captures
// success-line output. On terminal-failed surfaces the embedded
// error envelope as a Go error; on success returns nil.
func waitForTask(ctx context.Context, cmd *cobra.Command, c *cpclient.Client, taskID uuid.UUID, timeout time.Duration) error {
	stderr := cmd.ErrOrStderr()
	dotsEmitted := 0
	task, err := c.WaitTask(ctx, taskID, cpclient.WaitOptions{
		Timeout: timeout,
		OnPoll: func(t cpclient.Task) {
			_, _ = io.WriteString(stderr, ".")
			dotsEmitted++
		},
	})
	if dotsEmitted > 0 {
		_, _ = io.WriteString(stderr, "\n")
	}
	if err != nil {
		return err
	}
	if !task.IsTerminal() {
		// WaitTask only returns when terminal or error; defensive
		// branch for future surface drift.
		return fmt.Errorf("wait task: non-terminal status %q", task.Status)
	}
	if task.Status == "success" {
		return nil
	}
	env, decErr := task.DecodeError()
	if decErr != nil || env == nil {
		return fmt.Errorf("task %s terminated with status %q (no error envelope)", taskID, task.Status)
	}
	return fmt.Errorf("task %s %s: %s: %s", taskID, task.Status, env.Code, env.Message)
}

// requireStringFlag fetches a string flag and rejects empty values as
// usage errors. The CLI forwards the raw string and the server
// resolves it (name-only for VM/Node; polymorphic for
// storage pools). Format validation happens at the resolver layer,
// not the CLI edge.
func requireStringFlag(cmd *cobra.Command, name string) (string, error) {
	raw, err := cmd.Flags().GetString(name)
	if err != nil {
		return "", err
	}
	if raw == "" {
		return "", fmt.Errorf("--%s is required", name)
	}
	return raw, nil
}

// outputFormat reads the --output flag (default "text"). Unknown
// values surface as usage errors so subcommands fail fast rather than
// rendering garbage.
func outputFormat(cmd *cobra.Command, defaultFormat string) (string, error) {
	raw, err := cmd.Flags().GetString(flagOutput)
	if err != nil {
		return "", err
	}
	if raw == "" {
		raw = defaultFormat
	}
	switch raw {
	case "text", "json", "table", "yaml":
		return raw, nil
	}
	return "", fmt.Errorf("--%s: unknown format %q (text, json, table, yaml)", flagOutput, raw)
}
