// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package cloudinit holds CLI-side helpers для reading и validating
// raw `#cloud-config` YAML the operator passes к `otherix template
// create --cloud-init` / `otherix vm create --cloud-init`. Both flags
// take а filesystem path OR the literal `-` к slurp stdin (operator
// UX iteration locks). Validation is best-effort: YAML syntax check
// + а warning if the body does not start с the `#cloud-config`
// directive cloud-init expects на the first line.
//
// The package intentionally does not import cpclient or cobra — it
// is а pure I/O + parsing primitive both commands consume.
package cloudinit

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// StdinSentinel is the magic path value that switches ReadSource от
// filesystem к stdin. Matches the conventional shell idiom (`cat - |
// cmd` or `cmd -` reads stdin).
const StdinSentinel = "-"

// ErrEmptyPath signals that the caller invoked ReadSource without
// supplying а path. Callers should check the flag was set before
// calling — the helper does not paper over а bug in the CLI layer.
var ErrEmptyPath = errors.New("cloudinit: empty path")

// stdinReader is the io.Reader ReadSource uses когда path equals
// StdinSentinel. Exposed for tests; production swaps it back к
// os.Stdin in TestMain teardown.
var stdinReader io.Reader = os.Stdin

// ReadSource returns the raw byte content of the cloud-init YAML the
// operator supplied. The path is either а regular filesystem path or
// the literal "-" to read stdin. Empty path returns ErrEmptyPath so
// the CLI caller can distinguish "operator passed nothing" от "operator
// passed something that failed к read".
func ReadSource(path string) ([]byte, error) {
	if path == "" {
		return nil, ErrEmptyPath
	}
	if path == StdinSentinel {
		raw, err := io.ReadAll(stdinReader)
		if err != nil {
			return nil, fmt.Errorf("cloudinit: read stdin: %w", err)
		}
		return raw, nil
	}
	raw, err := os.ReadFile(path) //nolint:gosec // operator-supplied path is the entire point of the flag
	if err != nil {
		return nil, fmt.Errorf("cloudinit: read %s: %w", path, err)
	}
	return raw, nil
}

// Validate runs the best-effort syntactic check on а cloud-init blob.
// Returns nil on valid YAML; the typed *Diagnostic on parse errors;
// nil + а non-empty warnings slice when the body is parseable but is
// missing the `#cloud-config` directive cloud-init expects on line 1.
// The CLI caller renders warnings к stderr — they do not block
// dispatch (locked decision: best-effort validation, not gate-keeping).
//
// Empty input is treated as valid с а warning so the operator can
// pass an empty file к explicitly clear а previously-set pre-bake
// (PATCH path); the resolver treats empty и absent identically.
func Validate(data []byte) (warnings []string, err error) {
	body := strings.TrimLeft(string(data), " \t\n")
	if body == "" {
		return []string{"cloud-init body is empty; resolver will treat this as no cloud-init"}, nil
	}
	var parsed any
	if uerr := yaml.Unmarshal(data, &parsed); uerr != nil {
		return nil, &Diagnostic{Phase: "yaml_parse", Cause: uerr}
	}
	firstLine := strings.SplitN(body, "\n", 2)[0]
	if !strings.HasPrefix(firstLine, "#cloud-config") {
		warnings = append(warnings,
			"cloud-init body does not start with `#cloud-config` directive; guest cloud-init may не parse it correctly")
	}
	return warnings, nil
}

// Diagnostic is the typed error Validate returns on syntactic failure.
// Phase names the stage that rejected the input ("yaml_parse" today,
// room для future schema checks); Cause is the underlying parser
// error. CLI callers print Diagnostic.Error() verbatim so the
// operator sees the parser's line/column hint when YAML is malformed.
type Diagnostic struct {
	Phase string
	Cause error
}

// Error renders the diagnostic для CLI output. Format kept short —
// the operator typically pipes the YAML themselves, so the noise of
// а stack trace would obscure the parser's line/column hint.
func (d *Diagnostic) Error() string {
	return fmt.Sprintf("cloudinit: %s: %v", d.Phase, d.Cause)
}

// Unwrap exposes the underlying parser error for errors.As callers
// that want к pattern-match on yaml.TypeError or similar.
func (d *Diagnostic) Unwrap() error { return d.Cause }
