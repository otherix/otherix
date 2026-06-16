// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package pool

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	"github.com/otherix/otherix/cmd/cli/internal/clierr"
	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

const (
	flagOutput  = "output"
	flagShowIDs = "show-ids"
	flagLimit   = "limit"
	flagCursor  = "cursor"
	flagNode    = "node"
	flagType    = "type"

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

// outputFormat reads the --output flag (default defaultFormat). The base
// formats text/json/table are always accepted; extra lists additional
// formats a command opts into (yaml, only on get/list, which project a
// manifest). Unknown values surface as usage errors so a mutating command
// does not silently accept yaml and print text.
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

// formatPoolAvailable renders the agent-reported `available_bytes`
// optionally followed by "(effective N free)" when the view-reported
// `available_bytes_effective` diverges. Divergence signals VM disks
// committed but not yet observed by the agent's scan. Suffix omitted
// when the two agree, keeping list tables noise-free under normal
// cluster conditions.
//
// Both nil → "-"; effective nil → bare raw (view emits NULL effective
// only when raw is NULL too, so this branch is academic — but kept
// defensive).
func formatPoolAvailable(raw, effective *int64) string {
	if raw == nil {
		return "-"
	}
	base := humanBytes(raw)
	if effective != nil && *effective != *raw {
		base += fmt.Sprintf(" (effective %s free)", humanBytes(effective))
	}
	return base
}

// humanBytes renders a byte count in IEC binary units (KiB / MiB /
// GiB / TiB). nil → "-". Used for capacity / available columns in
// list tables — operators want "80GiB" rather than the raw integer.
func humanBytes(n *int64) string {
	if n == nil {
		return "-"
	}
	v := *n
	const (
		kib int64 = 1024
		mib       = kib * 1024
		gib       = mib * 1024
		tib       = gib * 1024
	)
	switch {
	case v >= tib:
		return fmt.Sprintf("%.1fTiB", float64(v)/float64(tib))
	case v >= gib:
		return fmt.Sprintf("%.1fGiB", float64(v)/float64(gib))
	case v >= mib:
		return fmt.Sprintf("%.1fMiB", float64(v)/float64(mib))
	case v >= kib:
		return fmt.Sprintf("%.1fKiB", float64(v)/float64(kib))
	default:
		return fmt.Sprintf("%dB", v)
	}
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

func boolYesNo(b bool) string {
	if b {
		return "yes"
	}
	return ""
}
