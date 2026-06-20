// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package manifest_test

import (
	"strings"
	"testing"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
	"github.com/otherix/otherix/cmd/cli/internal/manifest"
)

// TestVMSpecPullPolicyRoundTrip drives the real emit (ProjectVM) and read-back
// (Parse + DecodeVMSpec) converters: a VM carrying image_pull_policy "always"
// must surface as spec.imagePullPolicy in `get -o yaml` and decode back to the
// same value on `create -f`. The manifest key is camelCase; the value stays
// the API wire form (no value-mapping layer).
func TestVMSpecPullPolicyRoundTrip(t *testing.T) {
	in := cpclient.VM{
		Name:            "web-1",
		ImageURL:        "https://ex.test/noble.img",
		Architecture:    "arm64",
		ImagePullPolicy: "always",
	}
	b, err := manifest.ProjectVM(in)
	if err != nil {
		t.Fatalf("ProjectVM: %v", err)
	}
	if !strings.Contains(string(b), "imagePullPolicy: always") {
		t.Errorf("projected manifest missing imagePullPolicy:\n%s", b)
	}

	docs, err := manifest.Parse(strings.NewReader(string(b)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("Parse returned %d docs, want 1", len(docs))
	}
	spec, err := manifest.DecodeVMSpec(docs[0])
	if err != nil {
		t.Fatalf("DecodeVMSpec: %v", err)
	}
	if spec.ImagePullPolicy != "always" {
		t.Errorf("round-tripped pull policy = %q, want always", spec.ImagePullPolicy)
	}
}
