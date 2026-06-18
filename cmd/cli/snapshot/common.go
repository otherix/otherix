// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package snapshot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	"github.com/otherix/otherix/cmd/cli/internal/clierr"
	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

const (
	flagOutput = "output"
	flagVM     = "vm"
	flagStatus = "status"
	flagLimit  = "limit"
	flagCursor = "cursor"

	flagWait        = "wait"
	flagWaitTimeout = "wait-timeout"

	defaultListLimit = 50
	defaultWaitTO    = 10 * time.Minute
)

func clientFromFlags(cmd *cobra.Command) (*cpclient.Client, error) {
	return cliauth.BuildClient(cmd)
}

// resolveSnapshotID turns the operator-supplied id argument into a full snapshot
// uuid. A full uuid is used verbatim (no list call). Anything else is treated as
// the short id the `snapshot list` table prints (the leading hex of the uuid):
// the global list is paged through (every snapshot the caller can see) and the
// snapshot whose full id has the given prefix is resolved. Exactly one match ->
// its full uuid; zero matches -> a "no snapshot with id prefix" error; two or
// more -> an "ambiguous id prefix" error so the operator re-runs with the full
// id. The prefix is matched case-insensitively against the canonical lowercase
// uuid the server returns.
func resolveSnapshotID(ctx context.Context, c *cpclient.Client, arg string) (string, error) {
	if _, err := uuid.Parse(arg); err == nil {
		return arg, nil
	}

	prefix := strings.ToLower(arg)
	var matches []string
	cursor := ""
	for {
		page, next, err := c.ListSnapshotsGlobal(ctx, cpclient.ListSnapshotsParams{
			Limit:  200,
			Cursor: cursor,
		})
		if err != nil {
			return "", classifyError(err)
		}
		for _, s := range page {
			if strings.HasPrefix(strings.ToLower(s.ID), prefix) {
				matches = append(matches, s.ID)
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}

	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("not_found: no snapshot with id prefix %q", arg)
	default:
		return "", fmt.Errorf("validation_failed: ambiguous id prefix %q, use the full id", arg)
	}
}

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

// printYAML writes the raw server JSON re-rendered as YAML to stdout. Used by
// `--output yaml`. JSONToYAML preserves the server's field names and value types
// (integers stay integers, not floats) and emits a trailing newline, so the
// output is a faithful, stable YAML view of the same projection `--output json`
// echoes.
func printYAML(cmd *cobra.Command, raw json.RawMessage) error {
	out, err := yaml.JSONToYAML(raw)
	if err != nil {
		return fmt.Errorf("convert json to yaml: %v", err)
	}
	printf(cmd, "%s", out)
	return nil
}

func classifyError(err error) error {
	return clierr.Classify(err)
}

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

// humanAge renders an RFC 3339 timestamp as a compact age relative to now
// (s / m / h / d), matching the `migration` / `node` / `pool` list views. An
// empty or unparseable input renders the "-" placeholder so the column never
// shows a raw timestamp or a Go zero time.
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

// humanBytes renders a byte count in IEC binary units (KiB / MiB / GiB / TiB)
// for the snapshot size columns; the JSON output keeps the raw bytes. Mirrors
// the migration package helper.
func humanBytes(n int64) string {
	const (
		kib int64 = 1024
		mib       = kib * 1024
		gib       = mib * 1024
		tib       = gib * 1024
	)
	switch {
	case n >= tib:
		return fmt.Sprintf("%.1fTiB", float64(n)/float64(tib))
	case n >= gib:
		return fmt.Sprintf("%.1fGiB", float64(n)/float64(gib))
	case n >= mib:
		return fmt.Sprintf("%.1fMiB", float64(n)/float64(mib))
	case n >= kib:
		return fmt.Sprintf("%.1fKiB", float64(n)/float64(kib))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// snapshotSize renders the summed blob size in IEC units, or "-" when the
// snapshot has not yet been measured (nil until status reaches ready).
func snapshotSize(b *int64) string {
	if b == nil {
		return "-"
	}
	return humanBytes(*b)
}

// dashIfEmpty renders the "-" placeholder for an empty cell.
func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// dashIfNil renders a nullable string column preferring the pointee, falling
// back to "-" when nil/empty. Used for the resolved vm_name / owner_display_name
// columns (nil only when the referenced VM / user row is gone).
func dashIfNil(s *string) string {
	if s == nil {
		return "-"
	}
	return dashIfEmpty(*s)
}

// shortID truncates a snapshot id to the first segment of its UUID (the 8
// leading hex chars, no hyphen) for the table view. The full UUID stays
// available via --output json; snapshot get / delete are UUID-keyed and take
// the full id. Strings shorter than 8 chars pass through unchanged. Mirrors the
// migration package helper.
func shortID(s string) string {
	if len(s) >= 8 {
		return s[:8]
	}
	return s
}

// parseTaskID parses the task id from the CP's AsyncTaskAccepted envelope.
// Surfaces a helpful error if the CP ever returns a malformed value
// (defensive - should not happen in practice).
func parseTaskID(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, fmt.Errorf("malformed task id %q: %v", raw, err)
	}
	return id, nil
}

// waitForTask polls the supplied taskID until it reaches a terminal state, the
// timeout fires, or ctx is cancelled. A single dot per poll is written to
// stderr so stdout stays parseable. On terminal-failed it surfaces the embedded
// error envelope as a Go error; on success it returns nil. Mirrors the vm
// package helper.
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
