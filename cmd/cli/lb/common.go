// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package lb

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	"github.com/otherix/otherix/cmd/cli/internal/clierr"
	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

const (
	flagOutput   = "output"
	flagShowIDs  = "show-ids"
	flagLimit    = "limit"
	flagCursor   = "cursor"
	flagPort     = "port"
	flagSelector = "selector"

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
// is no next page.
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

// classifyError maps a cpclient error to a stable-prefixed operator
// message. Delegates to clierr.Classify (shared with the other CLI
// surfaces); kept as a thin wrapper so call sites stay untouched.
func classifyError(err error) error {
	return clierr.Classify(err)
}

// outputFormat reads the --output flag (default defaultFormat). The base
// formats text/json/table are always accepted; extra lists additional formats
// a command opts into (lb get/list opt into "yaml", the apply-ready manifest
// projection). Unknown values surface as usage errors so a mutating command
// does not silently accept an unsupported format and print text.
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

// parseSelector turns "app=web,tier=fe" into a map; a bare key or empty value is
// an error. Duplicate keys are rejected.
func parseSelector(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		if !ok || k == "" || v == "" {
			return nil, fmt.Errorf("invalid selector term %q: want key=value", pair)
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("duplicate selector key %q", k)
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil, errors.New("selector must have at least one key=value")
	}
	return out, nil
}

// formatSelector renders a selector map as a stable, comma-joined
// "k=v,k=v" string (keys sorted) for the text output.
func formatSelector(sel map[string]string) string {
	if len(sel) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(sel))
	for k := range sel {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+sel[k])
	}
	return strings.Join(parts, ",")
}
