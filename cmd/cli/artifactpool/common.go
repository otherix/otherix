// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package artifactpool

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	"github.com/otherix/otherix/cmd/cli/internal/clierr"
	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

const (
	flagOutput = "output"
	flagLimit  = "limit"
	flagCursor = "cursor"
	flagForce  = "force"

	flagReplicationFactor = "replication-factor"
	flagMembers           = "members"

	defaultListLimit = 20
)

func clientFromFlags(cmd *cobra.Command) (*cpclient.Client, error) {
	return cliauth.BuildClient(cmd)
}

func printf(cmd *cobra.Command, format string, args ...any) {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), format, args...)
}

// classifyError maps a cpclient error to a stable-prefixed operator message.
// Delegates to clierr.Classify (shared with the other CLI surfaces).
func classifyError(err error) error {
	return clierr.Classify(err)
}

// outputFormat reads the --output flag (default defaultFormat). The base
// formats text/json/table are always accepted. Unknown values surface as
// usage errors so a command does not silently accept an unsupported format.
func outputFormat(cmd *cobra.Command, defaultFormat string) (string, error) {
	raw, err := cmd.Flags().GetString(flagOutput)
	if err != nil {
		return "", err
	}
	if raw == "" {
		raw = defaultFormat
	}
	allowed := []string{"text", "json", "table"}
	if slices.Contains(allowed, raw) {
		return raw, nil
	}
	return "", fmt.Errorf("--%s: unknown format %q (%s)", flagOutput, raw, strings.Join(allowed, ", "))
}

// printJSONIndent re-marshals v two-space indented to the command's stdout.
func printJSONIndent(cmd *cobra.Command, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal json: %v", err)
	}
	printf(cmd, "%s\n", raw)
	return nil
}

// rfString renders a replication_factor raw JSON value
// (an integer or the string "all") as a plain operator-readable token. The
// integer form prints as its digits; the string form prints unquoted.
func rfString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "-"
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var buf bytes.Buffer
	_ = json.Compact(&buf, raw)
	return buf.String()
}

// membersString renders the membership object as an operator-readable token:
// "all" when all_nodes is set, otherwise the comma-joined node-name list (or
// "<none>" when empty).
func membersString(ap cpclient.ArtifactPool) string {
	if ap.Membership.AllNodes {
		return "all"
	}
	if len(ap.Membership.Nodes) == 0 {
		return "<none>"
	}
	return strings.Join(ap.Membership.Nodes, ",")
}

// stdinIsTTY reports whether stdin is a terminal. Tests redirect stdin to a
// pipe / file -> returns false -> confirmation is skipped without --force.
// Parallel of cmd/cli/network.stdinIsTTY.
func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// readYes reads a single line from stdin and returns true on a y/Y prefix.
// Empty input or anything else -> false (the safer side).
func readYes() bool {
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
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
