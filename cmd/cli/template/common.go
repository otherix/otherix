// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package template

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// Local flag names. The endpoint / token / cluster / config persistent
// flags are inherited from the root command — these are the strictly-
// local flags this group owns.
const (
	flagOutput  = "output"
	flagShowIDs = "show-ids"
	flagLimit   = "limit"
	flagCursor  = "cursor"
	flagVisible = "visibility"
	// flagArch is the short-form architecture flag. The prior
	// `--architecture` was renamed to `--arch` for conciseness; the
	// rename is a clean break (no alias) since the surface is still
	// pre-prod. Both `template create --arch` (template-attribute) and
	// `template list --arch` (filter) share this constant.
	flagArch        = "arch"
	flagOSFamily    = "os-family"
	flagWait        = "wait"
	flagWaitTimeout = "wait-timeout"

	defaultListLimit = 20
	defaultWaitTO    = 10 * time.Minute
)

func clientFromFlags(cmd *cobra.Command) (*cpclient.Client, error) {
	return cliauth.BuildClient(cmd)
}

// printf writes to cmd.OutOrStdout, swallowing the Write error — used
// for routine status output where the only failure mode is a closed
// pipe (caller already gone).
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

// classifyError mirrors cmd/cli/vm.classifyError so subcommands surface
// the same operator-shaped category prefixes ("api_error", "request_…").
// Kept package-local since extracting it would have only two consumers
// at this point — promotion to a shared helper waits until a third
// group needs it.
func classifyError(err error) error {
	var apiErr *cpclient.APIError
	if errors.As(err, &apiErr) {
		return fmt.Errorf("%s", apiErr.Error())
	}
	msg := err.Error()
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("request_timeout: %s", msg)
	case strings.Contains(msg, "connection refused"):
		return fmt.Errorf("connection_refused: %s", msg)
	case strings.Contains(msg, "tls:") || strings.Contains(msg, "x509:"):
		return fmt.Errorf("tls_handshake_failed: %s", msg)
	default:
		return fmt.Errorf("request_failed: %s", msg)
	}
}

// outputFormat reads --output (default supplied by caller). Unknown
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
	case "text", "json", "table":
		return raw, nil
	}
	return "", fmt.Errorf("--%s: unknown format %q (text, json, table)", flagOutput, raw)
}

// parseTaskID parses a task id from the CP's AsyncTaskAccepted envelope.
// Mirrors cmd/cli/vm.parseTaskID — kept package-local pending promotion
// when a third caller appears.
func parseTaskID(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, fmt.Errorf("malformed task id %q: %v", raw, err)
	}
	return id, nil
}

// waitForTask polls taskID until it reaches a terminal state, the
// timeout fires, or ctx is cancelled. Mirrors cmd/cli/vm.waitForTask —
// emits stderr dots on every poll so stdout stays parseable. Returns
// nil on success, a wrapped envelope error on failed/cancelled
// terminals.
func waitForTask(ctx context.Context, cmd *cobra.Command, c *cpclient.Client, taskID uuid.UUID, timeout time.Duration) error {
	stderr := cmd.ErrOrStderr()
	dotsEmitted := 0
	task, err := c.WaitTask(ctx, taskID, cpclient.WaitOptions{
		Timeout: timeout,
		OnPoll: func(_ cpclient.Task) {
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

// humanAge renders an RFC 3339 timestamp string as kubectl-style
// relative age: "2d", "5h", "30m". Empty / unparseable timestamps fall
// back to "-" so the table column stays well-typed.
func humanAge(rfc3339 string) string {
	if rfc3339 == "" {
		return "-"
	}
	t, err := time.Parse(time.RFC3339Nano, rfc3339)
	if err != nil {
		t, err = time.Parse(time.RFC3339, rfc3339)
		if err != nil {
			return "-"
		}
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
