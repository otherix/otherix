// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package network

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

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
	flagType    = "type"

	defaultListLimit = 20
)

func clientFromFlags(cmd *cobra.Command) (*cpclient.Client, error) {
	return cliauth.BuildClient(cmd)
}

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

// classifyError maps a cpclient error to a stable-prefixed operator
// message. Delegates to clierr.Classify (shared with the other CLI
// surfaces); kept as a thin wrapper so call sites stay untouched.
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

// orDash returns the dereferenced string, or "-" when the pointer is
// nil. Used for the optional subnet / gateway columns.
func orDash(s *string) string {
	if s == nil || *s == "" {
		return "-"
	}
	return *s
}

func boolYesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// dashIfNil renders an optional string as its value, or "-" when nil or empty.
// Used for table columns (e.g. CIDR) that are absent on networks without a subnet.
func dashIfNil(s *string) string {
	if s == nil || *s == "" {
		return "-"
	}
	return *s
}
