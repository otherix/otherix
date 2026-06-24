// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package manifest_test

import (
	"strings"
	"testing"

	"github.com/otherix/otherix/cmd/cli/internal/manifest"
)

// orderingSrc lists VM before StoragePool before Network on purpose, to
// prove BuildCreatePlan emits a stable Network -> StoragePool -> VM
// order regardless of document order. The ordering is cosmetic now (VM
// admission defers pool/network references, so submit order no longer
// affects correctness); the test only pins the deterministic-log shape.
const orderingSrc = `apiVersion: otherix/v1
kind: VM
metadata: { name: web-1 }
spec: { imageURL: https://x/u.qcow2, arch: arm64, pool: pool-dev, network: net-dev }
---
apiVersion: otherix/v1
kind: StoragePool
metadata: { name: pool-dev }
spec: { path: /opt/p, nodeList: [node-1, node-2] }
---
apiVersion: otherix/v1
kind: Network
metadata: { name: net-dev }
spec: { type: bridge, bridgeName: br0 }
`

// TestBuildCreatePlanStableOrderAndExpansion pins the deterministic
// (cosmetic) Network -> StoragePool -> VM apply order and the nodeList
// expansion. The order is no longer required for a successful apply -
// admission defers references - so this guards log determinism, not a
// submission dependency.
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
	if plan[3].VM == nil || plan[3].VM.Name != "web-1" || plan[3].VM.Pool != "pool-dev" {
		t.Errorf("op[3] vm = %+v, want web-1/pool-dev", plan[3].VM)
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
	if plan[1].PoolNode != "node-1" || plan[3].Name != "net-dev" {
		t.Errorf("delete targets mismatch: %+v", plan)
	}
}

func TestNetworkSpecDecodesDNS(t *testing.T) {
	// dns is a pointer in the params: an explicit dns: false in a manifest is
	// sent verbatim (it must reach the server, not be defaulted back to dhcp),
	// dns: true is sent, and an omitted dns leaves the pointer nil so the
	// server applies its per-type default.
	base := "apiVersion: otherix/v1\nkind: Network\nmetadata: { name: n }\nspec:\n  type: bridge\n  managed: true\n  bridgeName: br0\n  subnet: 10.0.0.0/24\n  dhcp: true\n"
	cases := []struct {
		name string
		src  string
		want *bool
	}{
		{"explicit-false", base + "  dns: false\n", boolPtr(false)},
		{"explicit-true", base + "  dns: true\n", boolPtr(true)},
		{"omitted", base, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			docs, err := manifest.Parse(strings.NewReader(tc.src))
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			plan, err := manifest.BuildCreatePlan(docs)
			if err != nil {
				t.Fatalf("BuildCreatePlan() error = %v", err)
			}
			if len(plan) != 1 || plan[0].Network == nil {
				t.Fatalf("plan = %+v, want 1 network op", plan)
			}
			got := plan[0].Network.DNS
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("Network.DNS = %v, want nil", *got)
			case tc.want != nil && got == nil:
				t.Errorf("Network.DNS = nil, want %v", *tc.want)
			case tc.want != nil && got != nil && *got != *tc.want:
				t.Errorf("Network.DNS = %v, want %v", *got, *tc.want)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }

func TestVMSpecToRequestMapsUserData(t *testing.T) {
	src := "apiVersion: otherix/v1\nkind: VM\nmetadata: { name: v }\nspec:\n  imageURL: https://x/u.qcow2\n  arch: arm64\n  userData: |\n    #cloud-config\n    packages: [htop]\n"
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

func TestVMSpecToRequestMapsNetworkConfig(t *testing.T) {
	src := "apiVersion: otherix/v1\nkind: VM\nmetadata: { name: v }\nspec:\n  imageURL: https://x/u.qcow2\n  arch: arm64\n  networkConfig: |\n    version: 2\n    ethernets: {}\n"
	docs, _ := manifest.Parse(strings.NewReader(src))
	plan, err := manifest.BuildCreatePlan(docs)
	if err != nil {
		t.Fatalf("BuildCreatePlan() error = %v", err)
	}
	if len(plan) != 1 {
		t.Fatalf("plan has %d ops, want 1", len(plan))
	}
	if plan[0].VM.NetworkConfig == nil || !strings.HasPrefix(*plan[0].VM.NetworkConfig, "version: 2") {
		t.Errorf("VM.NetworkConfig = %v, want inline network-config", plan[0].VM.NetworkConfig)
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

func TestBuildCreatePlanVMDefaultsVCPUsAndMemory(t *testing.T) {
	// A minimal VM manifest omits vcpus/memoryMB. The wire fields have no
	// omitempty and the server rejects 0 (vcpus must be [1,128], memory_mb
	// [128,524288]), so the manifest path must apply the same client-side
	// defaults the `vm create` flags do, matching the doc promise that
	// everything but imageURL/arch is "optional with server/CLI defaults".
	src := "apiVersion: otherix/v1\nkind: VM\nmetadata: { name: v }\nspec: { imageURL: https://x/u.qcow2, arch: arm64 }\n"
	docs, _ := manifest.Parse(strings.NewReader(src))
	plan, err := manifest.BuildCreatePlan(docs)
	if err != nil {
		t.Fatalf("BuildCreatePlan() error = %v", err)
	}
	if len(plan) != 1 {
		t.Fatalf("plan has %d ops, want 1", len(plan))
	}
	if plan[0].VM.VCPUs != 2 {
		t.Errorf("VM.VCPUs = %d, want 2 (default)", plan[0].VM.VCPUs)
	}
	if plan[0].VM.MemoryMB != 2048 {
		t.Errorf("VM.MemoryMB = %d, want 2048 (default)", plan[0].VM.MemoryMB)
	}
}

func TestBuildCreatePlanVMKeepsExplicitVCPUsAndMemory(t *testing.T) {
	// Explicit values must survive: defaulting only fills a zero.
	src := "apiVersion: otherix/v1\nkind: VM\nmetadata: { name: v }\nspec: { imageURL: https://x/u.qcow2, arch: arm64, vcpus: 8, memoryMB: 16384 }\n"
	docs, _ := manifest.Parse(strings.NewReader(src))
	plan, err := manifest.BuildCreatePlan(docs)
	if err != nil {
		t.Fatalf("BuildCreatePlan() error = %v", err)
	}
	if plan[0].VM.VCPUs != 8 || plan[0].VM.MemoryMB != 16384 {
		t.Errorf("VM vcpus/memory = %d/%d, want 8/16384", plan[0].VM.VCPUs, plan[0].VM.MemoryMB)
	}
}

func TestVMCreateOp_SnapshotSourced(t *testing.T) {
	// snapshot-sourced: SourceSnapshotID set, ImageURL/Arch empty.
	snapSrc := "apiVersion: otherix/v1\nkind: VM\nmetadata: { name: v }\nspec: { sourceSnapshotID: snap-1, vcpus: 4, memoryMB: 4096, pool: fast }\n"
	docs, _ := manifest.Parse(strings.NewReader(snapSrc))
	plan, err := manifest.BuildCreatePlan(docs)
	if err != nil {
		t.Fatalf("BuildCreatePlan(snapshot) error = %v", err)
	}
	if len(plan) != 1 || plan[0].VM == nil {
		t.Fatalf("plan = %+v, want 1 VM op", plan)
	}
	vm := plan[0].VM
	if vm.SourceSnapshotID != "snap-1" {
		t.Errorf("VM.SourceSnapshotID = %q, want snap-1", vm.SourceSnapshotID)
	}
	if vm.ImageURL != "" {
		t.Errorf("VM.ImageURL = %q, want empty (snapshot-sourced)", vm.ImageURL)
	}
	if vm.Arch != "" {
		t.Errorf("VM.Arch = %q, want empty (snapshot-sourced)", vm.Arch)
	}
	if vm.Pool != "fast" || vm.VCPUs != 4 || vm.MemoryMB != 4096 {
		t.Errorf("VM placement/sizing = pool=%q vcpus=%d memory=%d, want fast/4/4096", vm.Pool, vm.VCPUs, vm.MemoryMB)
	}

	// round-trip: a `vm get -o yaml` dump carries arch alongside
	// sourceSnapshotID. The build must DROP arch (snapshot is authoritative)
	// so the round-trip applies cleanly past the server's field_from_snapshot
	// admission.
	rtSrc := "apiVersion: otherix/v1\nkind: VM\nmetadata: { name: v }\nspec: { sourceSnapshotID: snap-1, arch: arm64, firmware: uefi, format: qcow2 }\n"
	docs, _ = manifest.Parse(strings.NewReader(rtSrc))
	plan, err = manifest.BuildCreatePlan(docs)
	if err != nil {
		t.Fatalf("BuildCreatePlan(round-trip) error = %v", err)
	}
	rt := plan[0].VM
	if rt.SourceSnapshotID != "snap-1" {
		t.Errorf("round-trip VM.SourceSnapshotID = %q, want snap-1", rt.SourceSnapshotID)
	}
	if rt.Arch != "" {
		t.Errorf("round-trip VM.Arch = %q, want empty (dropped: snapshot authoritative)", rt.Arch)
	}
	if rt.Firmware != "" || rt.Format != "" {
		t.Errorf("round-trip VM.Firmware=%q Format=%q, want empty (dropped)", rt.Firmware, rt.Format)
	}

	// image-sourced: ImageURL/Arch forwarded as before, no SourceSnapshotID.
	imgSrc := "apiVersion: otherix/v1\nkind: VM\nmetadata: { name: v }\nspec: { imageURL: https://x/u.qcow2, arch: arm64 }\n"
	docs, _ = manifest.Parse(strings.NewReader(imgSrc))
	plan, err = manifest.BuildCreatePlan(docs)
	if err != nil {
		t.Fatalf("BuildCreatePlan(image) error = %v", err)
	}
	img := plan[0].VM
	if img.ImageURL != "https://x/u.qcow2" || img.Arch != "arm64" {
		t.Errorf("image VM imageURL=%q arch=%q, want https://x/u.qcow2/arm64", img.ImageURL, img.Arch)
	}
	if img.SourceSnapshotID != "" {
		t.Errorf("image VM.SourceSnapshotID = %q, want empty", img.SourceSnapshotID)
	}
}

func TestBuildCreatePlanNetworkMapsDhcp(t *testing.T) {
	src := "apiVersion: otherix/v1\nkind: Network\nmetadata: { name: net-dhcp }\nspec: { type: overlay, egress: nat, subnet: 10.50.0.0/24, dhcp: true }\n"
	docs, err := manifest.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	plan, err := manifest.BuildCreatePlan(docs)
	if err != nil {
		t.Fatalf("BuildCreatePlan() error = %v", err)
	}
	if len(plan) != 1 || plan[0].Network == nil {
		t.Fatalf("plan = %+v, want 1 network op", plan)
	}
	if !plan[0].Network.Dhcp {
		t.Errorf("Network.Dhcp = false, want true")
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
