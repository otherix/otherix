// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package manifest_test

import (
	"strings"
	"testing"

	"github.com/otherix/otherix/cmd/cli/internal/manifest"
)

// orderingSrc lists VM before StoragePool before Network on purpose, to
// prove BuildCreatePlan reorders to Network -> StoragePool -> VM.
const orderingSrc = `apiVersion: otherix/v1
kind: VM
metadata: { name: web-1 }
spec: { imageURL: https://x/u.qcow2, arch: arm64, pool: pool-mvp, network: net-mvp }
---
apiVersion: otherix/v1
kind: StoragePool
metadata: { name: pool-mvp }
spec: { path: /opt/p, nodeList: [node-1, node-2] }
---
apiVersion: otherix/v1
kind: Network
metadata: { name: net-mvp }
spec: { type: bridge, bridgeName: br0 }
`

func TestBuildCreatePlanOrderingAndExpansion(t *testing.T) {
	docs, err := manifest.Parse(strings.NewReader(orderingSrc))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	plan, err := manifest.BuildCreatePlan(docs)
	if err != nil {
		t.Fatalf("BuildCreatePlan() error = %v", err)
	}
	wantKinds := []string{manifest.KindNetwork, manifest.KindStoragePool, manifest.KindStoragePool, manifest.KindVM}
	if len(plan) != len(wantKinds) {
		t.Fatalf("plan has %d ops, want %d", len(plan), len(wantKinds))
	}
	for i, w := range wantKinds {
		if plan[i].Kind != w {
			t.Errorf("op[%d].Kind = %q, want %q", i, plan[i].Kind, w)
		}
	}
	if plan[1].Pool == nil || plan[1].Pool.Node != "node-1" {
		t.Errorf("op[1] pool node = %+v, want node-1", plan[1].Pool)
	}
	if plan[2].Pool == nil || plan[2].Pool.Node != "node-2" {
		t.Errorf("op[2] pool node = %+v, want node-2", plan[2].Pool)
	}
	if plan[3].VM == nil || plan[3].VM.Name != "web-1" || plan[3].VM.Pool != "pool-mvp" {
		t.Errorf("op[3] vm = %+v, want web-1/pool-mvp", plan[3].VM)
	}
}

func TestBuildDeletePlanReverseOrder(t *testing.T) {
	docs, err := manifest.Parse(strings.NewReader(orderingSrc))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	plan, err := manifest.BuildDeletePlan(docs)
	if err != nil {
		t.Fatalf("BuildDeletePlan() error = %v", err)
	}
	wantKinds := []string{manifest.KindVM, manifest.KindStoragePool, manifest.KindStoragePool, manifest.KindNetwork}
	if len(plan) != len(wantKinds) {
		t.Fatalf("delete plan has %d targets, want %d", len(plan), len(wantKinds))
	}
	for i, w := range wantKinds {
		if plan[i].Kind != w {
			t.Errorf("target[%d].Kind = %q, want %q", i, plan[i].Kind, w)
		}
	}
	if plan[1].PoolNode != "node-1" || plan[3].Name != "net-mvp" {
		t.Errorf("delete targets mismatch: %+v", plan)
	}
}

func TestVMSpecToRequestMapsCloudInit(t *testing.T) {
	src := "apiVersion: otherix/v1\nkind: VM\nmetadata: { name: v }\nspec:\n  imageURL: https://x/u.qcow2\n  arch: arm64\n  cloudInit: |\n    #cloud-config\n    packages: [htop]\n"
	docs, _ := manifest.Parse(strings.NewReader(src))
	plan, err := manifest.BuildCreatePlan(docs)
	if err != nil {
		t.Fatalf("BuildCreatePlan() error = %v", err)
	}
	if len(plan) != 1 {
		t.Fatalf("plan has %d ops, want 1", len(plan))
	}
	if plan[0].VM.UserData == nil || !strings.HasPrefix(*plan[0].VM.UserData, "#cloud-config") {
		t.Errorf("VM.UserData = %v, want inline cloud-config", plan[0].VM.UserData)
	}
}

func TestBuildCreatePlanDedupesNodeList(t *testing.T) {
	src := "apiVersion: otherix/v1\nkind: StoragePool\nmetadata: { name: p }\nspec: { path: /x, nodeList: [n1, n1, n2] }\n"
	docs, _ := manifest.Parse(strings.NewReader(src))
	plan, err := manifest.BuildCreatePlan(docs)
	if err != nil {
		t.Fatalf("BuildCreatePlan() error = %v", err)
	}
	if len(plan) != 2 {
		t.Fatalf("plan has %d ops, want 2 (n1 deduped)", len(plan))
	}
	if plan[0].Pool.Node != "n1" || plan[1].Pool.Node != "n2" {
		t.Errorf("nodes = %s,%s, want n1,n2", plan[0].Pool.Node, plan[1].Pool.Node)
	}
}

func TestBuildCreatePlanVMNodePointer(t *testing.T) {
	src := "apiVersion: otherix/v1\nkind: VM\nmetadata: { name: v }\nspec: { imageURL: https://x/u.qcow2, arch: arm64, node: node-9 }\n"
	docs, _ := manifest.Parse(strings.NewReader(src))
	plan, err := manifest.BuildCreatePlan(docs)
	if err != nil {
		t.Fatalf("BuildCreatePlan() error = %v", err)
	}
	if len(plan) != 1 {
		t.Fatalf("plan has %d ops, want 1", len(plan))
	}
	if plan[0].VM.Node == nil || *plan[0].VM.Node != "node-9" {
		t.Errorf("VM.Node = %v, want pointer to node-9", plan[0].VM.Node)
	}
}
