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

	flagPublish       = "publish"
	flagPublishedPort = "publish-port"
	flagNoPublish     = "no-publish"
	flagSourceCIDR    = "source-cidr"

	flagHealthPort               = "health-port"
	flagHealthInterval           = "health-interval"
	flagHealthTimeout            = "health-timeout"
	flagHealthHealthyThreshold   = "health-healthy-threshold"
	flagHealthUnhealthyThreshold = "health-unhealthy-threshold"

	defaultListLimit = 20
)

// registerHealthCheckFlags adds the five optional active-health-check flags
// to a create/update command. All are plain ints; only the ones the operator
// sets are sent (see healthCheckFromFlags).
func registerHealthCheckFlags(cmd *cobra.Command) {
	cmd.Flags().Int(flagHealthPort, 0, "health-check TCP port to probe on each backend (default: follow --port)")
	cmd.Flags().Int(flagHealthInterval, 0, "seconds between health probes (1..300, default 10)")
	cmd.Flags().Int(flagHealthTimeout, 0, "per-probe connect timeout in seconds (1..60, default 2)")
	cmd.Flags().Int(flagHealthHealthyThreshold, 0, "consecutive successes before a backend is healthy (1..10, default 2)")
	cmd.Flags().Int(flagHealthUnhealthyThreshold, 0, "consecutive failures before a backend is unhealthy (1..10, default 3)")
}

// healthCheckFromFlags builds a *cpclient.HealthCheck from the --health-*
// flags, carrying only the sub-fields the operator set so the CP applies its
// default for each omitted one. Returns (nil, nil) when no --health-* flag was
// supplied, so the whole health_check block is omitted from the request.
func healthCheckFromFlags(cmd *cobra.Command) (*cpclient.HealthCheck, error) {
	var hc cpclient.HealthCheck
	binds := []struct {
		flag string
		dst  **int
	}{
		{flagHealthPort, &hc.Port},
		{flagHealthInterval, &hc.IntervalSeconds},
		{flagHealthTimeout, &hc.TimeoutSeconds},
		{flagHealthHealthyThreshold, &hc.HealthyThreshold},
		{flagHealthUnhealthyThreshold, &hc.UnhealthyThreshold},
	}
	for _, b := range binds {
		if !cmd.Flags().Changed(b.flag) {
			continue
		}
		v, err := cmd.Flags().GetInt(b.flag)
		if err != nil {
			return nil, err
		}
		*b.dst = &v
	}
	// An explicit --health-port 0 is the follow-the-traffic-port sentinel on the
	// server, but 0 violates the OpenAPI minimum:1 for the wire field. Follow is
	// meant to be selected by OMITTING the flag, so drop an explicit 0 and never
	// put port:0 on the wire.
	if hc.Port != nil && *hc.Port == 0 {
		hc.Port = nil
	}
	// Gate on real content, not just the changed bit: dropping an explicit port:0
	// can leave the struct empty even though a flag was "changed". If no sub-field
	// survives, omit the whole health_check block so the CP keeps its defaults.
	if hc.Port == nil && hc.IntervalSeconds == nil && hc.TimeoutSeconds == nil &&
		hc.HealthyThreshold == nil && hc.UnhealthyThreshold == nil {
		return nil, nil
	}
	return &hc, nil
}

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
