// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
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

// printNextCursor prints a copy-pasteable next-page hint for a cursor-paginated
// table listing. The cursor is opaque base64, so framing it as the exact next
// command reads far better than a bare "next_cursor: <blob>". No-op when there
// is no next page (the last page prints nothing).
func printNextCursor(cmd *cobra.Command, next string) {
	if next == "" {
		return
	}
	printf(cmd, "\nMore results - next page (re-add any filters):\n  %s --cursor %s\n", cmd.CommandPath(), next)
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

// waitForVMPhase polls the named VM until its status.phase reaches a
// terminal state (running, or error/failed), the timeout fires, or ctx
// is cancelled. Admission is split from scheduling: a freshly-created VM
// is `pending` until the CP reconcile loop binds it, then `creating`
// until the agent reports the runtime up, then `running`. This helper
// drives that wait off the public VM projection (there is no task to
// poll any more). Each poll's current scheduling reason is written to
// cmd.ErrOrStderr so stdout stays parseable; the failure terminal
// surfaces as a Go error.
func waitForVMPhase(ctx context.Context, cmd *cobra.Command, c *cpclient.Client, name string, timeout time.Duration) error {
	stderr := cmd.ErrOrStderr()
	loopCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	const (
		initial  = 1 * time.Second
		maxDelay = 5 * time.Second
	)
	delay := initial
	lastReason := ""
	for {
		vm, _, err := c.VM(loopCtx, name)
		if err != nil {
			var apiErr *cpclient.APIError
			if errors.As(err, &apiErr) && !apiErr.IsRetryable() {
				return err
			}
			if !errors.As(err, &apiErr) && loopCtx.Err() != nil {
				return fmt.Errorf("wait vm: %w", loopCtx.Err())
			}
			// Transient — fall through to sleep + retry.
		} else {
			if vm.Status.Reason != "" && vm.Status.Reason != lastReason {
				lastReason = vm.Status.Reason
				_, _ = fmt.Fprintf(stderr, "%s: %s\n", vm.Status.Phase, vm.Status.Reason)
			}
			if vm.Status.IsTerminalPhase() {
				if vm.Status.Phase == "running" {
					return nil
				}
				return fmt.Errorf("vm %s entered phase %q: %s: %s",
					name, vm.Status.Phase, vm.Status.Reason, vm.Status.Message)
			}
		}

		if err := sleepVMCtx(loopCtx, delay); err != nil {
			return fmt.Errorf("wait vm: %w", err)
		}
		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
		}
	}
}

// sleepVMCtx blocks for d or until ctx is done, whichever fires first.
// Returns ctx.Err() on cancellation, nil on full sleep.
func sleepVMCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// humanAge renders an RFC 3339 timestamp as a compact age relative to now
// (s / m / h / d), matching the `node` and `pool` list views. An empty or
// unparseable input renders the "-" placeholder so the column never shows a
// raw timestamp or a Go zero time.
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

// outputFormat reads the --output flag (default defaultFormat). The base
// formats text/json/table are always accepted; extra lists additional
// formats a command opts into (yaml, only on get/list, which project a
// manifest). Unknown values surface as usage errors so subcommands fail
// fast rather than rendering garbage -- a mutating command does not
// silently accept yaml and print text.
func outputFormat(cmd *cobra.Command, defaultFormat string, extra ...string) (string, error) {
	raw, err := cmd.Flags().GetString(flagOutput)
	if err != nil {
		return "", err
	}
	if raw == "" {
		raw = defaultFormat
	}
	allowed := append([]string{"text", "json", "table"}, extra...)
	if slices.Contains(allowed, raw) {
		return raw, nil
	}
	return "", fmt.Errorf("--%s: unknown format %q (%s)", flagOutput, raw, strings.Join(allowed, ", "))
}
