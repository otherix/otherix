// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/clierr"
	"github.com/otherix/otherix/cmd/cli/internal/cloudinit"
	"github.com/otherix/otherix/cmd/cli/internal/manifest"
)

// validateManifestCloudInit runs the shared cloud-init validator over
// every VM op carrying inline cloudInit. A parse error fails the whole
// command before any resource is created (mirroring vm create
// --cloud-init); non-blocking warnings (e.g. a body not starting with
// `#cloud-config`) are written to stderr.
func validateManifestCloudInit(cmd *cobra.Command, plan []manifest.CreateOp) error {
	for _, op := range plan {
		if op.Kind != manifest.KindVM || op.VM == nil || op.VM.UserData == nil {
			continue
		}
		warnings, err := cloudinit.Validate([]byte(*op.VM.UserData))
		if err != nil {
			return fmt.Errorf("manifest VM %q: cloud-init: %w", op.Name, err)
		}
		for _, w := range warnings {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: VM %q cloud-init: %s\n", op.Name, w)
		}
	}
	return nil
}

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
	kind      string
	name      string
	note      string // e.g. "task <id>" or "node node-1"
	err       error
	committed bool   // server create/delete call itself succeeded
	taskID    string // VM create: accepted task id, for --wait
	poolID    string // StoragePool create: created instance id, for --wait
}

// cpErr classifies an error returned by a cpclient call into the CLI's
// stable "<category>: <detail>" form (so operator scripts can grep
// connection_refused:/request_timeout:/etc.), matching the rest of the
// CLI. Returns nil for nil.
func cpErr(err error) error {
	if err == nil {
		return nil
	}
	return clierr.Classify(err)
}

// renderSummary prints one line per operation and returns a non-nil
// error if any operation failed (so the process exits non-zero).
// Success lines go to stdout; failure lines go to stderr.
func renderSummary(cmd *cobra.Command, verb string, results []docResult) error {
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()
	failed := 0
	for _, r := range results {
		label := r.kind + "/" + r.name
		if r.note != "" {
			label += " (" + r.note + ")"
		}
		if r.err != nil {
			failed++
			if r.committed {
				// the resource WAS created/changed server-side; a
				// follow-up (e.g. --wait) did not converge. Distinct from
				// a create/delete that never happened.
				_, _ = fmt.Fprintf(errOut, "%s %s but not ready: %v\n", verb, label, r.err)
			} else {
				_, _ = fmt.Fprintf(errOut, "%s failed: %s: %v\n", verb, label, r.err)
			}
			continue
		}
		_, _ = fmt.Fprintf(out, "%s %s\n", verb, label)
	}
	if failed > 0 {
		return fmt.Errorf("%s -f: %d of %d documents failed", verb, failed, len(results))
	}
	return nil
}
