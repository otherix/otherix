// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/manifest"
)

// readManifestDocs reads every -f source (file path, or "-" for stdin),
// concatenates them with `---`, and parses the combined stream. At
// least one source is required.
func readManifestDocs(cmd *cobra.Command, files []string) ([]manifest.Document, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("at least one -f/--filename is required")
	}
	var parts []string
	for _, f := range files {
		data, err := readOneSource(cmd, f)
		if err != nil {
			return nil, err
		}
		parts = append(parts, string(data))
	}
	combined := strings.Join(parts, "\n---\n")
	return manifest.Parse(strings.NewReader(combined))
}

// readOneSource reads a single -f source: a file path, or "-" for stdin.
func readOneSource(cmd *cobra.Command, f string) ([]byte, error) {
	if f == "-" {
		return io.ReadAll(cmd.InOrStdin())
	}
	data, err := os.ReadFile(f) //nolint:gosec // operator-supplied manifest path is the entire point of the flag
	if err != nil {
		return nil, fmt.Errorf("read %s: %v", f, err)
	}
	return data, nil
}

// docResult records the outcome of one fan-out operation for the
// summary. A nil err means success.
type docResult struct {
	kind   string
	name   string
	note   string // e.g. "task <id>" or "node node-1"
	err    error
	taskID string // VM create: accepted task id, for --wait
	poolID string // StoragePool create: created instance id, for --wait
}

// renderSummary prints one line per operation and returns a non-nil
// error if any operation failed (so the process exits non-zero).
func renderSummary(cmd *cobra.Command, verb string, results []docResult) error {
	failed := 0
	for _, r := range results {
		label := r.kind + "/" + r.name
		if r.note != "" {
			label += " (" + r.note + ")"
		}
		if r.err != nil {
			failed++
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s failed: %s: %v\n", verb, label, r.err)
			continue
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", verb, label)
	}
	if failed > 0 {
		return fmt.Errorf("%s -f: %d of %d documents failed", verb, failed, len(results))
	}
	return nil
}
