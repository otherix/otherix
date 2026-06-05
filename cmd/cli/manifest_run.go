// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/clierr"
	"github.com/otherix/otherix/cmd/cli/internal/cloudinit"
	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
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

// maxManifestBytes caps a single -f source (file or stdin), guarding
// against unbounded reads (a device node, a runaway pipe, a huge file).
// Package var (not a const) so tests can lower it; do NOT add
// t.Parallel() to package-main tests that touch it - it would race.
var maxManifestBytes int64 = 64 << 20 // 64 MiB

// readManifestDocs reads every -f source (file path, or "-" for stdin),
// concatenates them with `---`, and parses the combined stream. At
// least one source is required; stdin may be used at most once.
func readManifestDocs(cmd *cobra.Command, files []string) ([]manifest.Document, error) {
	if len(files) == 0 {
		return nil, errors.New("at least one -f/--filename is required")
	}
	var parts []string
	stdinUsed := false
	for _, f := range files {
		if f == "-" {
			if stdinUsed {
				return nil, errors.New("-f - may be given at most once")
			}
			stdinUsed = true
		}
		data, err := readOneSource(cmd, f)
		if err != nil {
			return nil, err
		}
		parts = append(parts, string(data))
	}
	combined := strings.Join(parts, "\n---\n")
	return manifest.Parse(strings.NewReader(combined))
}

// readOneSource reads a single -f source. "-" reads stdin (refused when
// stdin is a terminal, so an operator who typed `-f -` with nothing
// piped gets a clear error instead of an indefinite hang). A file path
// must be a regular file within the size cap; directories, FIFOs, and
// device nodes are rejected up front rather than read (a FIFO/device
// would otherwise hang or read unbounded).
func readOneSource(cmd *cobra.Command, f string) ([]byte, error) {
	if f == "-" {
		if file, ok := cmd.InOrStdin().(*os.File); ok {
			if fi, err := file.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
				return nil, errors.New("-f -: refusing to read manifests from a terminal; pipe stdin or pass a file path")
			}
		}
		return readCapped(cmd.InOrStdin())
	}
	fi, err := os.Stat(f)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", f)
	}
	if fi.Size() > maxManifestBytes {
		return nil, fmt.Errorf("%s: manifest exceeds the %d-byte limit", f, maxManifestBytes)
	}
	data, err := os.ReadFile(f) //nolint:gosec // operator-supplied manifest path
	if err != nil {
		return nil, err
	}
	return data, nil
}

// readCapped reads from r up to maxManifestBytes, erroring if exceeded
// (stdin has no stat size, so the cap is enforced via a LimitReader).
func readCapped(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxManifestBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxManifestBytes {
		return nil, fmt.Errorf("manifest exceeds the %d-byte limit", maxManifestBytes)
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
// CLI. Returns nil for nil. Used for the --wait path (GetPoolByID /
// WaitTask), where every error is a genuine transport / poll fault and
// the transport category is correct.
func cpErr(err error) error {
	if err == nil {
		return nil
	}
	return clierr.Classify(err)
}

// fanoutErr renders a fan-out error for the per-document summary. Typed
// domain conflicts (a resource still in use, or already existing) are
// surfaced with their own message - never wrapped in clierr's transport
// "request_failed:" category, which scripts use to detect transport
// faults. Everything else is classified for the stable category prefix.
func fanoutErr(err error) error {
	if err == nil {
		return nil
	}
	var poolBlocked *cpclient.ErrPoolBlocked
	if errors.As(err, &poolBlocked) {
		return fmt.Errorf("blocked: %s", formatBlocking(poolBlocked.Resources))
	}
	var netBlocked *cpclient.ErrNetworkBlocked
	if errors.As(err, &netBlocked) {
		return fmt.Errorf("blocked: %s", formatBlocking(netBlocked.Resources))
	}
	if errors.Is(err, cpclient.ErrPoolExists) || errors.Is(err, cpclient.ErrNetworkExists) {
		// already-exists is a clean domain message; keep it verbatim,
		// no transport prefix.
		return err
	}
	return clierr.Classify(err)
}

// formatBlocking renders a blocking-resources map deterministically as
// "k=v, k=v" with sorted keys (avoids Go's map[...] ordering noise).
func formatBlocking(res map[string]int64) string {
	keys := make([]string, 0, len(res))
	for k := range res {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, res[k]))
	}
	return strings.Join(parts, ", ")
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
