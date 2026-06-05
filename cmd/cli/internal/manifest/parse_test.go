// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package manifest_test

import (
	"strings"
	"testing"

	"github.com/otherix/otherix/cmd/cli/internal/manifest"
)

func TestParseMultiDoc(t *testing.T) {
	src := `apiVersion: otherix/v1
kind: Network
metadata: { name: net-mvp }
spec: { type: bridge, bridgeName: br0 }
---
apiVersion: otherix/v1
kind: VM
metadata: { name: web-1 }
spec: { imageURL: https://x/u.qcow2, arch: arm64 }
`
	docs, err := manifest.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("Parse() returned %d docs, want 2", len(docs))
	}
	if docs[0].Kind != manifest.KindNetwork || docs[0].Name != "net-mvp" {
		t.Errorf("doc0 = {%q,%q}, want {Network,net-mvp}", docs[0].Kind, docs[0].Name)
	}
	if docs[1].Kind != manifest.KindVM || docs[1].Name != "web-1" {
		t.Errorf("doc1 = {%q,%q}, want {VM,web-1}", docs[1].Kind, docs[1].Name)
	}
}

func TestParseRejectsUnknownKind(t *testing.T) {
	src := "apiVersion: otherix/v1\nkind: Gadget\nmetadata: { name: x }\nspec: {}\n"
	_, err := manifest.Parse(strings.NewReader(src))
	if err == nil || !strings.Contains(err.Error(), "Gadget") {
		t.Errorf("Parse() error = %v, want mention of unknown kind 'Gadget'", err)
	}
}

func TestParseRejectsWrongAPIVersion(t *testing.T) {
	src := "apiVersion: v2\nkind: VM\nmetadata: { name: x }\nspec: {}\n"
	_, err := manifest.Parse(strings.NewReader(src))
	if err == nil || !strings.Contains(err.Error(), "apiVersion") {
		t.Errorf("Parse() error = %v, want mention of apiVersion", err)
	}
}

func TestParseRejectsEmptyName(t *testing.T) {
	src := "apiVersion: otherix/v1\nkind: VM\nmetadata: {}\nspec: {}\n"
	_, err := manifest.Parse(strings.NewReader(src))
	if err == nil || !strings.Contains(err.Error(), "metadata.name") {
		t.Errorf("Parse() error = %v, want mention of metadata.name", err)
	}
}

func TestParseSkipsEmptyDocuments(t *testing.T) {
	src := "---\napiVersion: otherix/v1\nkind: Network\nmetadata: { name: n }\nspec: { type: bridge, bridgeName: br0 }\n---\n"
	docs, err := manifest.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("Parse() returned %d docs, want 1 (empty docs skipped)", len(docs))
	}
}
