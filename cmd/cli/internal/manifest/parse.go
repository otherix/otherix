// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package manifest

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// Parse decodes a multi-document YAML stream into Documents, splitting
// on the `---` separator. Empty documents (e.g. a trailing `---`) are
// skipped. Each document's header (apiVersion, kind, metadata.name) is
// validated; the spec body is left as a raw node for per-kind decode.
func Parse(r io.Reader) ([]Document, error) {
	dec := yaml.NewDecoder(r)
	var docs []Document
	for i := 0; ; i++ {
		var node yaml.Node
		err := dec.Decode(&node)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("manifest: document %d: parse: %v", i, err)
		}
		if isEmptyDocument(&node) {
			continue // empty document, e.g. a bare or trailing `---`
		}
		doc, err := decodeHeader(&node, i)
		if err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
	return docs, nil
}

// isEmptyDocument reports whether a decoded document node carries no
// content. A bare or trailing `---` decodes to a DocumentNode whose
// sole child is a null scalar; such documents are skipped rather than
// validated.
func isEmptyDocument(node *yaml.Node) bool {
	if node.Kind == 0 {
		return true
	}
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return true
		}
		child := node.Content[0]
		return child.Tag == "!!null"
	}
	return false
}

// decodeHeader pulls apiVersion/kind/metadata.name + the spec node out
// of a single document node and validates the header fields.
func decodeHeader(node *yaml.Node, index int) (Document, error) {
	var raw struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
		Metadata   struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
		Spec yaml.Node `yaml:"spec"`
	}
	if err := node.Decode(&raw); err != nil {
		return Document{}, fmt.Errorf("manifest: document %d: decode: %v", index, err)
	}
	if raw.APIVersion != APIVersionV1 {
		return Document{}, fmt.Errorf("manifest: document %d: apiVersion %q is not supported (want %q)", index, raw.APIVersion, APIVersionV1)
	}
	switch raw.Kind {
	case KindNetwork, KindStoragePool, KindVM:
	default:
		return Document{}, fmt.Errorf("manifest: document %d: unknown kind %q (want Network, StoragePool, or VM)", index, raw.Kind)
	}
	if strings.TrimSpace(raw.Metadata.Name) == "" {
		return Document{}, fmt.Errorf("manifest: document %d (%s): metadata.name is required", index, raw.Kind)
	}
	return Document{
		Index:      index,
		APIVersion: raw.APIVersion,
		Kind:       raw.Kind,
		Name:       strings.TrimSpace(raw.Metadata.Name),
		Spec:       raw.Spec,
	}, nil
}
