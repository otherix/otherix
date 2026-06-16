// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package migration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	"github.com/otherix/otherix/cmd/cli/internal/clierr"
	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// humanBytes renders a byte count in IEC binary units (KiB / MiB / GiB /
// TiB). Parallels the node / pool package helpers but takes a value (the
// migration stats counters are non-nullable int64). Used by the `migration
// get` statistics section so operators see "2.1GiB" rather than the raw byte
// count; the JSON output keeps the raw bytes.
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

const (
	flagOutput = "output"
	flagVM     = "vm"
	flagNode   = "node"
	flagLimit  = "limit"
	flagCursor = "cursor"

	defaultListLimit = 20
)

func clientFromFlags(cmd *cobra.Command) (*cpclient.Client, error) {
	return cliauth.BuildClient(cmd)
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

// dashIfEmpty renders the "-" placeholder for an empty cell.
func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// shortID truncates a migration id to the first segment of its UUID (the 8
// leading hex chars, no hyphen) for display. The CP resolves a unique prefix
// (>= 8 chars) back to the full row, so this short form is enough to pass to
// `migration get`/`cancel`; an ambiguous prefix returns 409 there. Strings
// shorter than 8 chars pass through unchanged.
func shortID(s string) string {
	if len(s) >= 8 {
		return s[:8]
	}
	return s
}

// vmLabel renders the VM column preferring the human name. vm_name should
// always resolve server-side, but if it is empty (a missing VM the CP logged
// rather than 500ing) the id is shown as a fallback so the column is never blank.
func vmLabel(m cpclient.Migration) string {
	if m.VMName != "" {
		return m.VMName
	}
	return dashIfEmpty(m.VMID)
}

// nodeLabel renders a node column preferring the human name. When the name is
// nil but the id is present (e.g. a force-deleted node whose row is gone but the
// migration still records the id), the id is shown so the operator is not left
// blind. Both absent renders the "-" placeholder.
func nodeLabel(name, id *string) string {
	if name != nil && *name != "" {
		return *name
	}
	if id != nil && *id != "" {
		return *id
	}
	return "-"
}
