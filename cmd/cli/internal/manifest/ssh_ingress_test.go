// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package manifest_test

import (
	"strings"
	"testing"

	"github.com/otherix/otherix/cmd/cli/internal/manifest"
)

// TestVMSpecSSHIngressManifest drives the real read path (Parse +
// BuildCreatePlan): a VM manifest with spec.sshIngressEnabled=true decodes
// and maps onto the create request's SSHIngressEnabled field, so `create -f`
// carries the per-VM opt-in. The manifest key is camelCase.
func TestVMSpecSSHIngressManifest(t *testing.T) {
	src := "apiVersion: otherix/v1\nkind: VM\nmetadata: { name: ssh-vm }\n" +
		"spec:\n  imageURL: https://x/u.qcow2\n  arch: arm64\n  sshIngressEnabled: true\n"

	docs, err := manifest.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	spec, err := manifest.DecodeVMSpec(docs[0])
	if err != nil {
		t.Fatalf("DecodeVMSpec: %v", err)
	}
	if !spec.SSHIngressEnabled {
		t.Fatalf("decoded SSHIngressEnabled = false, want true")
	}

	plan, err := manifest.BuildCreatePlan(docs)
	if err != nil {
		t.Fatalf("BuildCreatePlan: %v", err)
	}
	if len(plan) != 1 {
		t.Fatalf("plan has %d ops, want 1", len(plan))
	}
	if plan[0].VM == nil || !plan[0].VM.SSHIngressEnabled {
		t.Errorf("plan VM request SSHIngressEnabled = false, want true")
	}
}

// TestVMSpecSSHIngressMutualExclusion confirms the manifest rejects a VM
// document that sets both sshIngressEnabled and cloudInitDisabled, mirroring
// the server's 400 and the CLI flag guard.
func TestVMSpecSSHIngressMutualExclusion(t *testing.T) {
	src := "apiVersion: otherix/v1\nkind: VM\nmetadata: { name: ssh-vm }\n" +
		"spec:\n  imageURL: https://x/u.qcow2\n  arch: arm64\n  sshIngressEnabled: true\n  cloudInitDisabled: true\n"

	docs, err := manifest.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := manifest.DecodeVMSpec(docs[0]); err == nil {
		t.Fatalf("DecodeVMSpec err = nil, want mutual-exclusion error")
	}
}
