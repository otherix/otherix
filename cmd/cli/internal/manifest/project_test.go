// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package manifest_test

import (
	"strings"
	"testing"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
	"github.com/otherix/otherix/cmd/cli/internal/manifest"
)

func TestProjectNetworkRoundTrips(t *testing.T) {
	n := cpclient.Network{Name: "net-mvp", Type: "bridge", BridgeName: "br0"}
	out, err := manifest.ProjectNetwork(n)
	if err != nil {
		t.Fatalf("ProjectNetwork() error = %v", err)
	}
	docs, err := manifest.Parse(strings.NewReader(string(out)))
	if err != nil {
		t.Fatalf("re-parse projected network: %v", err)
	}
	if _, err := manifest.BuildCreatePlan(docs); err != nil {
		t.Fatalf("projected network is not apply-ready: %v", err)
	}
	if !strings.Contains(string(out), "kind: Network") || !strings.Contains(string(out), "bridgeName: br0") {
		t.Errorf("projection missing expected fields:\n%s", out)
	}
	if strings.Contains(string(out), "status") || strings.Contains(string(out), "createdAt") || strings.Contains(string(out), "id:") {
		t.Errorf("projection leaked server fields:\n%s", out)
	}
}

func TestProjectMultiDocSeparator(t *testing.T) {
	a := cpclient.Network{Name: "a", Type: "bridge", BridgeName: "br0"}
	b := cpclient.Network{Name: "b", Type: "bridge", BridgeName: "br1"}
	doc1, _ := manifest.ProjectNetwork(a)
	doc2, _ := manifest.ProjectNetwork(b)
	joined := manifest.JoinDocuments([][]byte{doc1, doc2})
	if strings.Count(string(joined), "---") != 1 {
		t.Errorf("JoinDocuments() separator count = %d, want 1:\n%s", strings.Count(string(joined), "---"), joined)
	}
}
