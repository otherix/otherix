// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package node

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

const (
	flagOutput  = "output"
	flagShowIDs = "show-ids"
	flagLimit   = "limit"
	flagCursor  = "cursor"
	flagArch    = "architecture"
	flagStatus  = "status"

	defaultListLimit = 20
)

func clientFromFlags(cmd *cobra.Command) (*cpclient.Client, error) {
	return cliauth.BuildClient(cmd)
}

func printf(cmd *cobra.Command, format string, args ...any) {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), format, args...)
}

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
	return "no"
}

// humanBytes renders a byte count in IEC binary units. nil → "-". Used
// by the system_disk rendering in `otherix node get` so operators see
// "80.5 GiB" rather than the raw integer. Parallels the pool package
// helper of the same name.
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
