// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package manifest_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/otherix/otherix/cmd/cli/internal/manifest"
)

func parseOne(t *testing.T, src string) manifest.Document {
	t.Helper()
	docs, err := manifest.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("Parse() returned %d docs, want 1", len(docs))
	}
	return docs[0]
}

func TestDecodeNetworkSpec(t *testing.T) {
	d := parseOne(t, "apiVersion: otherix/v1\nkind: Network\nmetadata: { name: n }\nspec: { type: bridge, bridgeName: br0 }\n")
	got, err := manifest.DecodeNetworkSpec(d)
	if err != nil {
		t.Fatalf("DecodeNetworkSpec() error = %v", err)
	}
	want := manifest.NetworkSpec{Type: "bridge", BridgeName: "br0"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("DecodeNetworkSpec() mismatch (-want +got):\n%s", diff)
	}
}

func TestDecodeNetworkSpecRejectsUnknownField(t *testing.T) {
	d := parseOne(t, "apiVersion: otherix/v1\nkind: Network\nmetadata: { name: n }\nspec: { type: bridge, bridgeName: br0, wat: 1 }\n")
	_, err := manifest.DecodeNetworkSpec(d)
	if err == nil || !strings.Contains(err.Error(), "wat") {
		t.Errorf("DecodeNetworkSpec() error = %v, want unknown-field 'wat'", err)
	}
}

func TestDecodeStoragePoolSpecNodeXorNodeList(t *testing.T) {
	both := parseOne(t, "apiVersion: otherix/v1\nkind: StoragePool\nmetadata: { name: p }\nspec: { path: /p, node: n1, nodeList: [n2] }\n")
	if _, err := manifest.DecodeStoragePoolSpec(both); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("both node+nodeList error = %v, want 'mutually exclusive'", err)
	}
	neither := parseOne(t, "apiVersion: otherix/v1\nkind: StoragePool\nmetadata: { name: p }\nspec: { path: /p }\n")
	if _, err := manifest.DecodeStoragePoolSpec(neither); err == nil || !strings.Contains(err.Error(), "node") {
		t.Errorf("neither node nor nodeList error = %v, want mention of node", err)
	}
}

func TestDecodeVMSpecRequiresImageAndArch(t *testing.T) {
	d := parseOne(t, "apiVersion: otherix/v1\nkind: VM\nmetadata: { name: v }\nspec: { arch: arm64 }\n")
	if _, err := manifest.DecodeVMSpec(d); err == nil || !strings.Contains(err.Error(), "imageURL") {
		t.Errorf("missing imageURL error = %v, want mention of imageURL", err)
	}
}

func TestDecodeVMSpecCloudInitMutualExclusion(t *testing.T) {
	d := parseOne(t, "apiVersion: otherix/v1\nkind: VM\nmetadata: { name: v }\nspec:\n  imageURL: https://x/u.qcow2\n  arch: arm64\n  cloudInit: \"#cloud-config\"\n  cloudInitDisabled: true\n")
	if _, err := manifest.DecodeVMSpec(d); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("cloudInit+disabled error = %v, want 'mutually exclusive'", err)
	}
}

func TestDecodeVMSpecRejectsDesiredPhaseUnsupported(t *testing.T) {
	d := parseOne(t, "apiVersion: otherix/v1\nkind: VM\nmetadata: { name: v }\nspec: { imageURL: https://x/u.qcow2, arch: arm64, desiredPhase: running }\n")
	if _, err := manifest.DecodeVMSpec(d); err == nil || !strings.Contains(err.Error(), "desiredPhase") {
		t.Errorf("DecodeVMSpec(desiredPhase set) error = %v, want rejection mentioning desiredPhase", err)
	}
}
