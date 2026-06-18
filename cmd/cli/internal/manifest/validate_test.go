// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package manifest_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/otherix/otherix/cmd/cli/internal/manifest"
)

func TestBuildCreatePlanRejectsNetworkWithoutType(t *testing.T) {
	// A typo'd top-level spec key (e.g. `specc:`) leaves the document with no
	// spec mapping, so the Network spec decodes empty. Unlike StoragePool
	// (path required) and VM (imageURL required), Network had no required
	// field, so the empty create slipped past the CLI edge and was caught
	// only by the server. The create path must reject a Network with no type.
	const m = "apiVersion: otherix/v1\nkind: Network\nmetadata: { name: n1 }\nspecc: { type: bridge, bridgeName: br0 }\n"
	docs, err := manifest.Parse(strings.NewReader(m))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	_, err = manifest.BuildCreatePlan(docs)
	if err == nil {
		t.Fatalf("BuildCreatePlan() = nil error, want rejection of a typeless Network")
	}
	if !strings.Contains(err.Error(), "spec.type is required") {
		t.Errorf("err = %v, want 'spec.type is required'", err)
	}
}

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

func TestDecodeStoragePoolSpecRejectsEmptyNodeListElement(t *testing.T) {
	d := parseOne(t, "apiVersion: otherix/v1\nkind: StoragePool\nmetadata: { name: p }\nspec: { path: /x, nodeList: [\"\", n2] }\n")
	if _, err := manifest.DecodeStoragePoolSpec(d); err == nil || !strings.Contains(err.Error(), "empty node") {
		t.Errorf("error = %v, want rejection of empty node name", err)
	}
}

func TestParseRejectsWhitespaceName(t *testing.T) {
	_, err := manifest.Parse(strings.NewReader("apiVersion: otherix/v1\nkind: VM\nmetadata: { name: \"   \" }\nspec: {}\n"))
	if err == nil || !strings.Contains(err.Error(), "metadata.name") {
		t.Errorf("error = %v, want metadata.name required for whitespace-only name", err)
	}
}

func TestDecodeVMSpecRequiresImageAndArch(t *testing.T) {
	d := parseOne(t, "apiVersion: otherix/v1\nkind: VM\nmetadata: { name: v }\nspec: { arch: arm64 }\n")
	if _, err := manifest.DecodeVMSpec(d); err == nil || !strings.Contains(err.Error(), "imageURL") {
		t.Errorf("missing imageURL error = %v, want mention of imageURL", err)
	}
}

func TestDecodeVMSpec_ImageXorSnapshot(t *testing.T) {
	// snapshot-only: no imageURL, no arch -> decodes cleanly with the
	// snapshot id set (arch comes from the snapshot manifest, not the spec).
	snapOnly := parseOne(t, "apiVersion: otherix/v1\nkind: VM\nmetadata: { name: v }\nspec: { sourceSnapshotID: snap-1 }\n")
	got, err := manifest.DecodeVMSpec(snapOnly)
	if err != nil {
		t.Fatalf("DecodeVMSpec(snapshot-only) error = %v, want nil", err)
	}
	if got.SourceSnapshotID != "snap-1" {
		t.Errorf("DecodeVMSpec(snapshot-only) SourceSnapshotID = %q, want snap-1", got.SourceSnapshotID)
	}

	// both imageURL and sourceSnapshotID -> rejected (exactly one).
	both := parseOne(t, "apiVersion: otherix/v1\nkind: VM\nmetadata: { name: v }\nspec: { imageURL: https://x/u.qcow2, arch: arm64, sourceSnapshotID: snap-1 }\n")
	if _, err := manifest.DecodeVMSpec(both); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("DecodeVMSpec(both) error = %v, want 'exactly one'", err)
	}

	// neither imageURL nor sourceSnapshotID -> rejected (exactly one).
	neither := parseOne(t, "apiVersion: otherix/v1\nkind: VM\nmetadata: { name: v }\nspec: { vcpus: 2 }\n")
	if _, err := manifest.DecodeVMSpec(neither); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("DecodeVMSpec(neither) error = %v, want 'exactly one'", err)
	}

	// image-sourced with arch -> OK (unchanged).
	img := parseOne(t, "apiVersion: otherix/v1\nkind: VM\nmetadata: { name: v }\nspec: { imageURL: https://x/u.qcow2, arch: arm64 }\n")
	if _, err := manifest.DecodeVMSpec(img); err != nil {
		t.Errorf("DecodeVMSpec(image+arch) error = %v, want nil", err)
	}

	// image-sourced without arch -> rejected (arch still required for image path).
	imgNoArch := parseOne(t, "apiVersion: otherix/v1\nkind: VM\nmetadata: { name: v }\nspec: { imageURL: https://x/u.qcow2 }\n")
	if _, err := manifest.DecodeVMSpec(imgNoArch); err == nil || !strings.Contains(err.Error(), "arch is required") {
		t.Errorf("DecodeVMSpec(image, no arch) error = %v, want 'arch is required'", err)
	}
}

func TestDecodeVMSpecUserDataMutualExclusion(t *testing.T) {
	d := parseOne(t, "apiVersion: otherix/v1\nkind: VM\nmetadata: { name: v }\nspec:\n  imageURL: https://x/u.qcow2\n  arch: arm64\n  userData: \"#cloud-config\"\n  cloudInitDisabled: true\n")
	if _, err := manifest.DecodeVMSpec(d); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("userData+disabled error = %v, want 'mutually exclusive'", err)
	}
}

func TestDecodeVMSpecNetworkConfigMutualExclusion(t *testing.T) {
	d := parseOne(t, "apiVersion: otherix/v1\nkind: VM\nmetadata: { name: v }\nspec:\n  imageURL: https://x/u.qcow2\n  arch: arm64\n  networkConfig: \"version: 2\"\n  cloudInitDisabled: true\n")
	if _, err := manifest.DecodeVMSpec(d); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("networkConfig+disabled error = %v, want 'mutually exclusive'", err)
	}
}

func TestDecodeRejectsNonMappingSpec(t *testing.T) {
	d := parseOne(t, "apiVersion: otherix/v1\nkind: Network\nmetadata: { name: n }\nspec: [a, b]\n")
	if _, err := manifest.DecodeNetworkSpec(d); err == nil || !strings.Contains(err.Error(), "mapping") {
		t.Errorf("DecodeNetworkSpec(list spec) error = %v, want 'spec must be a mapping'", err)
	}
}

func TestDecodeVMSpecRejectsDesiredPhaseUnsupported(t *testing.T) {
	d := parseOne(t, "apiVersion: otherix/v1\nkind: VM\nmetadata: { name: v }\nspec: { imageURL: https://x/u.qcow2, arch: arm64, desiredPhase: running }\n")
	if _, err := manifest.DecodeVMSpec(d); err == nil || !strings.Contains(err.Error(), "desiredPhase") {
		t.Errorf("DecodeVMSpec(desiredPhase set) error = %v, want rejection mentioning desiredPhase", err)
	}
}

func TestDecodeAcceptsAliasMappingSpec(t *testing.T) {
	src := "apiVersion: otherix/v1\nkind: Network\nmetadata: { name: a }\nspec: &s { type: bridge, bridgeName: br0 }\n---\napiVersion: otherix/v1\nkind: Network\nmetadata: { name: b }\nspec: *s\n"
	docs, err := manifest.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, d := range docs {
		got, err := manifest.DecodeNetworkSpec(d)
		if err != nil {
			t.Fatalf("DecodeNetworkSpec(%s): %v", d.Name, err)
		}
		if got.Type != "bridge" || got.BridgeName != "br0" {
			t.Errorf("alias spec decoded = %+v, want bridge/br0", got)
		}
	}
}

func TestDecodeAliasSpecStillRejectsUnknownField(t *testing.T) {
	src := "apiVersion: otherix/v1\nkind: Network\nmetadata: { name: a }\nspec: &s { type: bridge, bridgeName: br0, bogus: 1 }\n---\napiVersion: otherix/v1\nkind: Network\nmetadata: { name: b }\nspec: *s\n"
	docs, _ := manifest.Parse(strings.NewReader(src))
	if _, err := manifest.DecodeNetworkSpec(docs[1]); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Errorf("alias-with-bogus error = %v, want unknown-field 'bogus'", err)
	}
}

func TestDecodeStoragePoolSpecRejectsWhitespaceNode(t *testing.T) {
	d := parseOne(t, "apiVersion: otherix/v1\nkind: StoragePool\nmetadata: { name: p }\nspec: { path: /x, node: \"   \" }\n")
	if _, err := manifest.DecodeStoragePoolSpec(d); err == nil {
		t.Errorf("whitespace-only node should be rejected (treated as empty)")
	}
}

func TestDecodeVMSpecTrimsPlacementWhitespace(t *testing.T) {
	// node/pool/network are identity references; padded values must not
	// leak into the (node, name) placement key, mirroring StoragePool.
	d := parseOne(t, "apiVersion: otherix/v1\nkind: VM\nmetadata: { name: v }\nspec: { imageURL: https://x/u.qcow2, arch: arm64, node: \"  node-9  \", pool: \"  fast  \", network: \"  br  \" }\n")
	got, err := manifest.DecodeVMSpec(d)
	if err != nil {
		t.Fatalf("DecodeVMSpec() error = %v", err)
	}
	if got.Node != "node-9" || got.Pool != "fast" || got.Network != "br" {
		t.Errorf("DecodeVMSpec() placement = node=%q pool=%q network=%q, want node-9/fast/br", got.Node, got.Pool, got.Network)
	}
}

func TestDecodeVMSpecTreatsWhitespaceNodeAsUnset(t *testing.T) {
	// A whitespace-only node must collapse to unset (no placement pin),
	// not be sent verbatim as a bogus node name.
	d := parseOne(t, "apiVersion: otherix/v1\nkind: VM\nmetadata: { name: v }\nspec: { imageURL: https://x/u.qcow2, arch: arm64, node: \"   \" }\n")
	got, err := manifest.DecodeVMSpec(d)
	if err != nil {
		t.Fatalf("DecodeVMSpec() error = %v", err)
	}
	if got.Node != "" {
		t.Errorf("DecodeVMSpec() node = %q, want empty (whitespace collapses to unset)", got.Node)
	}
}

func TestDecodeNetworkSpecTrimsBridgeName(t *testing.T) {
	d := parseOne(t, "apiVersion: otherix/v1\nkind: Network\nmetadata: { name: n }\nspec: { type: bridge, bridgeName: \"  br0  \" }\n")
	got, err := manifest.DecodeNetworkSpec(d)
	if err != nil {
		t.Fatalf("DecodeNetworkSpec() error = %v", err)
	}
	if got.BridgeName != "br0" {
		t.Errorf("DecodeNetworkSpec() bridgeName = %q, want trimmed br0", got.BridgeName)
	}
}
