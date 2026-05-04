// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Command openapi-normalize converts OpenAPI 3.1 nullable type arrays
// (`type: [X, "null"]` or `type: ["null", X]`) into the 3.0-equivalent
// form (`type: X` plus `nullable: true`) so a downstream codegen tool
// that supports only OpenAPI 3.0 can consume the spec. It also
// rewrites a top-level `openapi: 3.1.x` version field to `openapi:
// 3.0.3`, since the schema-level normalisation makes the document
// 3.0-compliant in content; the version field is the only remaining
// signal that drives the oapi-codegen 3.1 warning.
//
// The tool walks the YAML AST in place and rewrites every two-element
// nullable type array. Multi-type unions that are not "X plus null" are
// rejected with a non-zero exit and a JSON-pointer-shaped path so the
// operator can locate the offending schema.
//
// Usage:
//
//	openapi-normalize <input.yaml> <output.yaml>
//
// Upstream blocker:
// https://github.com/oapi-codegen/oapi-codegen/issues/373.
package main

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: openapi-normalize <input.yaml> <output.yaml>")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run reads inputPath, normalises every nullable type array in place,
// and writes the result to outputPath.
func run(inputPath, outputPath string) error {
	data, err := os.ReadFile(inputPath) //nolint:gosec // CLI tool: input path is the operator-supplied argument.
	if err != nil {
		return fmt.Errorf("read %s: %v", inputPath, err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("parse %s: %v", inputPath, err)
	}
	rewriteOpenAPIVersion(&root)
	if _, err := normalize(&root, ""); err != nil {
		return err
	}
	out, err := yaml.Marshal(&root)
	if err != nil {
		return fmt.Errorf("marshal: %v", err)
	}
	if err := os.WriteFile(outputPath, out, 0o600); err != nil { //nolint:gosec // CLI tool: output path is the operator-supplied argument.
		return fmt.Errorf("write %s: %v", outputPath, err)
	}
	return nil
}

// rewriteOpenAPIVersion downgrades a top-level `openapi: 3.1.x` field
// to `openapi: 3.0.3`. The schema-level normalisation already makes
// the document 3.0-compatible in content, so the version field is
// the only remaining trigger for the oapi-codegen 3.1 warning. No-op
// when the field is missing or already 3.0.x.
func rewriteOpenAPIVersion(root *yaml.Node) {
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return
	}
	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return
	}
	_, ver := findMappingValue(doc, "openapi")
	if ver == nil || ver.Kind != yaml.ScalarNode {
		return
	}
	if strings.HasPrefix(ver.Value, "3.1.") {
		ver.Value = "3.0.3"
	}
}

// normalize walks n recursively and rewrites every two-element nullable
// type array into 3.0-equivalent form. It returns the number of
// rewrites and an error for malformed inputs (multi-type unions that
// are not "X plus null", or a nullable type array on a schema that
// already declares nullable: true).
func normalize(n *yaml.Node, path string) (int, error) {
	if n == nil {
		return 0, nil
	}
	switch n.Kind {
	case yaml.DocumentNode:
		return normalizeChildren(n.Content, path)
	case yaml.MappingNode:
		return normalizeMapping(n, path)
	case yaml.SequenceNode:
		return normalizeSequence(n, path)
	}
	return 0, nil
}

// normalizeMapping handles the MappingNode case: rewrites a nullable
// type array attached to this mapping, then recurses into every value.
func normalizeMapping(n *yaml.Node, path string) (int, error) {
	count := 0
	if delta, err := rewriteNullableTypeArray(n, path); err != nil {
		return count, err
	} else {
		count += delta
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key := n.Content[i].Value
		delta, err := normalize(n.Content[i+1], joinPath(path, key))
		if err != nil {
			return count, err
		}
		count += delta
	}
	return count, nil
}

// rewriteNullableTypeArray collapses `type: [X, "null"]` on n (when
// present) into `type: X` plus an appended `nullable: true`. Returns
// the number of rewrites (0 or 1) and an error for malformed
// inputs.
func rewriteNullableTypeArray(n *yaml.Node, path string) (int, error) {
	typeIdx, typeSeq := findMappingValue(n, "type")
	if typeSeq == nil || typeSeq.Kind != yaml.SequenceNode {
		return 0, nil
	}
	scalar, err := simplifyNullableType(typeSeq, path)
	if err != nil {
		return 0, err
	}
	if scalar == nil {
		return 0, nil
	}
	if hasMappingKey(n, "nullable") {
		return 0, fmt.Errorf("schema at %s already declares nullable; cannot also use type: [..., null]", displayPath(path))
	}
	n.Content[typeIdx+1] = scalar
	n.Content = append(n.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "nullable"},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"},
	)
	return 1, nil
}

// normalizeSequence recurses into each element of a sequence,
// indexing the path by position.
func normalizeSequence(n *yaml.Node, path string) (int, error) {
	count := 0
	for i, c := range n.Content {
		delta, err := normalize(c, fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			return count, err
		}
		count += delta
	}
	return count, nil
}

// normalizeChildren recurses into a slice of child nodes, leaving the
// path untouched (used for DocumentNode where children are top-level).
func normalizeChildren(children []*yaml.Node, path string) (int, error) {
	count := 0
	for _, c := range children {
		delta, err := normalize(c, path)
		if err != nil {
			return count, err
		}
		count += delta
	}
	return count, nil
}

// findMappingValue returns the index of the key in m.Content along with
// the value node, or (-1, nil) when the key is missing.
func findMappingValue(m *yaml.Node, key string) (int, *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i, m.Content[i+1]
		}
	}
	return -1, nil
}

// hasMappingKey reports whether m carries a top-level entry for key.
func hasMappingKey(m *yaml.Node, key string) bool {
	idx, _ := findMappingValue(m, key)
	return idx >= 0
}

// simplifyNullableType returns a scalar node carrying the non-null type
// when seq is exactly `[X, "null"]` or `["null", X]`. It returns
// (nil, nil) when seq is not a nullable type array at all (caller
// leaves the sequence alone). It returns an error when seq has the
// shape of a type array but is not "X plus null" — for example
// `[string, integer]` or three-element arrays.
func simplifyNullableType(seq *yaml.Node, path string) (*yaml.Node, error) {
	if len(seq.Content) == 0 {
		return nil, fmt.Errorf("schema at %s has empty type array", displayPath(path))
	}
	if !allScalars(seq.Content) {
		return nil, fmt.Errorf("schema at %s has non-scalar entry in type array", displayPath(path))
	}
	if len(seq.Content) != 2 {
		values := scalarValues(seq.Content)
		return nil, fmt.Errorf("schema at %s has multi-type union %v; only [X, \"null\"] is supported", displayPath(path), values)
	}
	var nonNull *yaml.Node
	hasNull := false
	for _, item := range seq.Content {
		if item.Value == "null" {
			hasNull = true
			continue
		}
		nonNull = item
	}
	if !hasNull || nonNull == nil {
		values := scalarValues(seq.Content)
		return nil, fmt.Errorf("schema at %s has multi-type union %v; only [X, \"null\"] is supported", displayPath(path), values)
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: nonNull.Value}, nil
}

func allScalars(nodes []*yaml.Node) bool {
	for _, n := range nodes {
		if n.Kind != yaml.ScalarNode {
			return false
		}
	}
	return true
}

func scalarValues(nodes []*yaml.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Value)
	}
	return out
}

// joinPath appends segment to a dotted JSON-pointer-shaped breadcrumb.
func joinPath(prefix, segment string) string {
	if prefix == "" {
		return segment
	}
	return prefix + "." + segment
}

// displayPath returns a non-empty form of path for diagnostic messages.
func displayPath(path string) string {
	if path == "" {
		return "<root>"
	}
	return path
}
