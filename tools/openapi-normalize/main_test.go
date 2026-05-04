// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestNormalize_Golden verifies that running the tool on each input
// fixture produces exactly the bytes of its matching golden fixture.
func TestNormalize_Golden(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		golden string
	}{
		{name: "simple", input: "simple_input.yaml", golden: "simple_golden.yaml"},
		{name: "reverse", input: "reverse_input.yaml", golden: "reverse_golden.yaml"},
		{name: "nested", input: "nested_input.yaml", golden: "nested_golden.yaml"},
		{name: "idempotent", input: "idempotent_input.yaml", golden: "idempotent_golden.yaml"},
		{name: "version", input: "version_input.yaml", golden: "version_golden.yaml"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := runFixture(t, c.input)
			want := readFixture(t, c.golden)
			if diff := cmp.Diff(string(want), string(got)); diff != "" {
				t.Errorf("normalize(%s) mismatch (-want +got):\n%s", c.input, diff)
			}
		})
	}
}

// TestNormalize_Errors verifies that malformed inputs produce a
// non-nil error whose message contains the expected substring. The
// message must include both the failure mode and a path to the
// offending schema so operators can locate it.
func TestNormalize_Errors(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantParts []string
	}{
		{
			name:      "multi_type_union",
			input:     "error_multi_union.yaml",
			wantParts: []string{"multi-type union", "properties.weird"},
		},
		{
			name:      "three_types",
			input:     "error_three_types.yaml",
			wantParts: []string{"multi-type union", "properties.too_many"},
		},
		{
			name:      "existing_nullable",
			input:     "error_with_existing_nullable.yaml",
			wantParts: []string{"already declares nullable", "properties.bad"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "out.yaml")
			err := run(filepath.Join("testdata", c.input), out)
			if err == nil {
				t.Fatalf("run(%s) = nil, want error", c.input)
			}
			msg := err.Error()
			for _, part := range c.wantParts {
				if !strings.Contains(msg, part) {
					t.Errorf("run(%s) error = %q, want substring %q", c.input, msg, part)
				}
			}
		})
	}
}

// TestNormalize_Idempotent verifies that normalising an
// already-normalised golden fixture is a no-op (byte-equal output).
func TestNormalize_Idempotent(t *testing.T) {
	goldens := []string{
		"simple_golden.yaml",
		"reverse_golden.yaml",
		"nested_golden.yaml",
		"idempotent_golden.yaml",
		"version_golden.yaml",
	}
	for _, g := range goldens {
		t.Run(g, func(t *testing.T) {
			first := runFixture(t, g)
			want := readFixture(t, g)
			if !bytes.Equal(first, want) {
				t.Errorf("normalize(%s) not idempotent: output differs from input", g)
			}
		})
	}
}

// TestNormalize_Deterministic verifies that running the tool twice on
// the same input yields byte-equal output.
func TestNormalize_Deterministic(t *testing.T) {
	inputs := []string{
		"simple_input.yaml",
		"reverse_input.yaml",
		"nested_input.yaml",
		"idempotent_input.yaml",
		"version_input.yaml",
	}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			first := runFixture(t, in)
			second := runFixture(t, in)
			if !bytes.Equal(first, second) {
				t.Errorf("normalize(%s) is not deterministic across runs", in)
			}
		})
	}
}

// runFixture runs the normaliser on a fixture under testdata/ and
// returns the resulting bytes. Fatal on tool error.
func runFixture(t *testing.T, fixture string) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out.yaml")
	if err := run(filepath.Join("testdata", fixture), out); err != nil {
		t.Fatalf("run(%s): %v", fixture, err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read %s: %v", out, err)
	}
	return data
}

// readFixture reads a file under testdata/. Fatal on read error.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read testdata/%s: %v", name, err)
	}
	return data
}
