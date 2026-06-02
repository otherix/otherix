// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package network

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
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
// message. *APIError surfaces verbatim ("api_error: <code>: <msg>");
// transport failures get a coarse classifier prefix. Mirror of
// cmd/cli/pool.classifyError.
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
