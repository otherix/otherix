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
	n := cpclient.Network{Name: "net-dev", Type: "bridge", BridgeName: "br0"}
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

func TestProjectNetworkRoundTripsMTUAndVLAN(t *testing.T) {
	vlan := 42
	n := cpclient.Network{Name: "net-dev", Type: "bridge", BridgeName: "br0", MTU: 9000, VlanTag: &vlan}
	out, err := manifest.ProjectNetwork(n)
	if err != nil {
		t.Fatalf("ProjectNetwork() error = %v", err)
	}
	docs, err := manifest.Parse(strings.NewReader(string(out)))
	if err != nil {
		t.Fatalf("re-parse projected network: %v", err)
	}
	plan, err := manifest.BuildCreatePlan(docs)
	if err != nil {
		t.Fatalf("projected network is not apply-ready: %v", err)
	}
	if len(plan) != 1 || plan[0].Network == nil {
		t.Fatalf("plan = %+v, want 1 network op", plan)
	}
	got := plan[0].Network
	if got.Mtu == nil || *got.Mtu != 9000 {
		t.Errorf("Network.Mtu = %v, want 9000", got.Mtu)
	}
	if got.VlanTag == nil || *got.VlanTag != 42 {
		t.Errorf("Network.VlanTag = %v, want 42", got.VlanTag)
	}
}

func TestProjectNetworkOverlayIsReappliable(t *testing.T) {
	// An overlay network's bridge_name/mtu/vlan are server-derived and the
	// create API forbids them for type=overlay. Projecting them verbatim
	// yields a manifest the server rejects, so the overlay projection must
	// emit only type + subnet (the valid overlay create body).
	subnet := "10.10.0.0/24"
	n := cpclient.Network{Name: "ovl", Type: "overlay", BridgeName: "otb100", MTU: 1390, Subnet: &subnet}
	out, err := manifest.ProjectNetwork(n)
	if err != nil {
		t.Fatalf("ProjectNetwork() error = %v", err)
	}
	if strings.Contains(string(out), "bridgeName") {
		t.Errorf("overlay projection must not emit bridgeName (server-forbidden):\n%s", out)
	}
	if strings.Contains(string(out), "mtu") {
		t.Errorf("overlay projection must not emit mtu (server-forbidden):\n%s", out)
	}
	if !strings.Contains(string(out), "type: overlay") || !strings.Contains(string(out), "subnet: 10.10.0.0/24") {
		t.Errorf("overlay projection missing type/subnet:\n%s", out)
	}
	docs, err := manifest.Parse(strings.NewReader(string(out)))
	if err != nil {
		t.Fatalf("re-parse overlay projection: %v", err)
	}
	if _, err := manifest.BuildCreatePlan(docs); err != nil {
		t.Fatalf("overlay projection not apply-ready: %v", err)
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

func TestProjectVMRoundTrips(t *testing.T) {
	v := cpclient.VM{Name: "vm1", ImageURL: "http://x/i.qcow2", Architecture: "amd64", VCPUs: 2, MemoryMB: 1024}
	out, err := manifest.ProjectVM(v)
	if err != nil {
		t.Fatalf("ProjectVM() error = %v", err)
	}
	docs, err := manifest.Parse(strings.NewReader(string(out)))
	if err != nil {
		t.Fatalf("re-parse projected VM: %v", err)
	}
	if _, err := manifest.BuildCreatePlan(docs); err != nil {
		t.Fatalf("projected VM is not apply-ready: %v", err)
	}
}

func TestProjectPoolInstanceRoundTrips(t *testing.T) {
	p := cpclient.Pool{Name: "pool1", Type: "local_dir", Path: "/opt/p", Node: "node-1"}
	out, err := manifest.ProjectPoolInstance(p)
	if err != nil {
		t.Fatalf("ProjectPoolInstance() error = %v", err)
	}
	docs, err := manifest.Parse(strings.NewReader(string(out)))
	if err != nil {
		t.Fatalf("re-parse projected pool instance: %v", err)
	}
	if _, err := manifest.BuildCreatePlan(docs); err != nil {
		t.Fatalf("projected pool instance is not apply-ready: %v", err)
	}
}

func TestProjectPoolConceptRoundTrips(t *testing.T) {
	c := cpclient.PoolConceptView{
		Name: "pool1",
		Instances: []cpclient.Pool{
			{Name: "pool1", Type: "local_dir", Path: "/opt/p", Node: "node-1"},
			{Name: "pool1", Type: "local_dir", Path: "/opt/p", Node: "node-2"},
		},
	}
	out, err := manifest.ProjectPoolConcept(c)
	if err != nil {
		t.Fatalf("ProjectPoolConcept() error = %v", err)
	}
	docs, err := manifest.Parse(strings.NewReader(string(out)))
	if err != nil {
		t.Fatalf("re-parse projected pool concept: %v", err)
	}
	plan, err := manifest.BuildCreatePlan(docs)
	if err != nil {
		t.Fatalf("projected pool concept is not apply-ready: %v", err)
	}
	// nodeList of 2 must expand back to 2 create ops.
	if len(plan) != 2 {
		t.Errorf("pool-concept round-trip expanded to %d ops, want 2", len(plan))
	}
}

func TestProjectPoolConceptDivergentPathsRoundTripsPerInstance(t *testing.T) {
	// path is a free per-instance column: instances of one pool name can
	// carry different per-node paths. A single nodeList doc with one path
	// would rewrite every node to the first instance's path on re-apply,
	// silently dropping the others. Divergent paths must project as one
	// document per instance so each node keeps its own path.
	c := cpclient.PoolConceptView{
		Name: "pool1",
		Instances: []cpclient.Pool{
			{Name: "pool1", Type: "local_dir", Path: "/opt/a", Node: "node-1"},
			{Name: "pool1", Type: "local_dir", Path: "/opt/b", Node: "node-2"},
		},
	}
	out, err := manifest.ProjectPoolConcept(c)
	if err != nil {
		t.Fatalf("ProjectPoolConcept() error = %v", err)
	}
	docs, err := manifest.Parse(strings.NewReader(string(out)))
	if err != nil {
		t.Fatalf("re-parse projected pool concept: %v", err)
	}
	plan, err := manifest.BuildCreatePlan(docs)
	if err != nil {
		t.Fatalf("projected pool concept is not apply-ready: %v", err)
	}
	if len(plan) != 2 {
		t.Fatalf("divergent-path concept expanded to %d ops, want 2", len(plan))
	}
	got := map[string]string{}
	for _, op := range plan {
		if op.Pool == nil {
			t.Fatalf("op %+v missing pool params", op)
		}
		got[op.Pool.Node] = op.Pool.Path
	}
	if got["node-1"] != "/opt/a" {
		t.Errorf("node-1 path = %q, want /opt/a", got["node-1"])
	}
	if got["node-2"] != "/opt/b" {
		t.Errorf("node-2 path = %q, want /opt/b", got["node-2"])
	}
}

// mustCreatePlan re-parses a projection and builds its create plan,
// failing the test if either step errors. Shared by the matrix-guard
// round-trip tests below.
func mustCreatePlan(t *testing.T, projected []byte) []manifest.CreateOp {
	t.Helper()
	docs, err := manifest.Parse(strings.NewReader(string(projected)))
	if err != nil {
		t.Fatalf("re-parse projection: %v", err)
	}
	plan, err := manifest.BuildCreatePlan(docs)
	if err != nil {
		t.Fatalf("projection not apply-ready: %v", err)
	}
	return plan
}

// TestProjectNetworkBridgeRoundTripPreservesEveryField is a matrix guard:
// every create-settable field the bridge Network view carries must survive
// `get -o yaml | create -f`. A new bridge field added to the view and create
// surface but not to ProjectNetwork fails here.
func TestProjectNetworkBridgeRoundTripPreservesEveryField(t *testing.T) {
	subnet, gateway, vlan := "10.0.0.0/24", "10.0.0.1", 100
	n := cpclient.Network{
		Name: "net-full", Type: "bridge", BridgeName: "br9",
		Managed: true, Egress: "nat",
		Subnet: &subnet, Gateway: &gateway, MTU: 9000, VlanTag: &vlan,
	}
	out, err := manifest.ProjectNetwork(n)
	if err != nil {
		t.Fatalf("ProjectNetwork() error = %v", err)
	}
	plan := mustCreatePlan(t, out)
	if len(plan) != 1 || plan[0].Network == nil {
		t.Fatalf("plan = %+v, want 1 network op", plan)
	}
	got := plan[0].Network
	if got.Type != "bridge" || got.BridgeName != "br9" {
		t.Errorf("type/bridgeName = %q/%q, want bridge/br9", got.Type, got.BridgeName)
	}
	if !got.Managed {
		t.Errorf("managed lost in round-trip")
	}
	if got.Egress != "nat" {
		t.Errorf("egress = %q, want nat", got.Egress)
	}
	if got.Subnet != "10.0.0.0/24" {
		t.Errorf("subnet = %q, want 10.0.0.0/24", got.Subnet)
	}
	if got.Gateway != "10.0.0.1" {
		t.Errorf("gateway = %q, want 10.0.0.1", got.Gateway)
	}
	if got.Mtu == nil || *got.Mtu != 9000 {
		t.Errorf("mtu = %v, want 9000", got.Mtu)
	}
	if got.VlanTag == nil || *got.VlanTag != 100 {
		t.Errorf("vlan = %v, want 100", got.VlanTag)
	}
}

// TestProjectVMRoundTripPreservesViewFields is a matrix guard: every
// create-settable field the VM view actually carries must survive the
// round-trip. (firmware/firmwareID/diskGiB/cloudInit are intentionally
// absent from the view and so cannot round-trip - see the docs caveat.)
func TestProjectVMRoundTripPreservesViewFields(t *testing.T) {
	node := "node-7"
	v := cpclient.VM{
		Name: "vm-full", ImageURL: "http://x/i.qcow2", ImageSHA256: "abc123",
		Architecture: "arm64", Format: "raw", Pool: "pool-a", Node: &node,
		Networks: []string{"net-a"}, VCPUs: 8, MemoryMB: 16384,
	}
	out, err := manifest.ProjectVM(v)
	if err != nil {
		t.Fatalf("ProjectVM() error = %v", err)
	}
	plan := mustCreatePlan(t, out)
	if len(plan) != 1 || plan[0].VM == nil {
		t.Fatalf("plan = %+v, want 1 vm op", plan)
	}
	got := plan[0].VM
	if got.ImageURL != "http://x/i.qcow2" || got.ImageSHA256 != "abc123" {
		t.Errorf("image url/sha = %q/%q", got.ImageURL, got.ImageSHA256)
	}
	if got.Arch != "arm64" || got.Format != "raw" {
		t.Errorf("arch/format = %q/%q, want arm64/raw", got.Arch, got.Format)
	}
	if got.Pool != "pool-a" {
		t.Errorf("pool = %q, want pool-a", got.Pool)
	}
	if got.Node == nil || *got.Node != "node-7" {
		t.Errorf("node = %v, want node-7", got.Node)
	}
	if got.Network != "net-a" {
		t.Errorf("network = %q, want net-a", got.Network)
	}
	if got.VCPUs != 8 || got.MemoryMB != 16384 {
		t.Errorf("vcpus/memoryMB = %d/%d, want 8/16384", got.VCPUs, got.MemoryMB)
	}
}

// TestProjectPoolInstanceRoundTripPreservesFields is a matrix guard for the
// flat per-instance pool projection: type, path, and node must all survive.
func TestProjectPoolInstanceRoundTripPreservesFields(t *testing.T) {
	p := cpclient.Pool{Name: "pool-x", Type: "local_dir", Path: "/srv/data", Node: "node-3"}
	out, err := manifest.ProjectPoolInstance(p)
	if err != nil {
		t.Fatalf("ProjectPoolInstance() error = %v", err)
	}
	plan := mustCreatePlan(t, out)
	if len(plan) != 1 || plan[0].Pool == nil {
		t.Fatalf("plan = %+v, want 1 pool op", plan)
	}
	got := plan[0].Pool
	if got.Name != "pool-x" || got.Type != "local_dir" || got.Path != "/srv/data" || got.Node != "node-3" {
		t.Errorf("pool = %+v, want {pool-x local_dir /srv/data node-3}", got)
	}
}

func TestProjectVMSingleNICOnly(t *testing.T) {
	v := cpclient.VM{Name: "vm1", ImageURL: "http://x/i.qcow2", Architecture: "amd64", VCPUs: 1, MemoryMB: 512, Networks: []string{"net-a", "net-b"}}
	out, err := manifest.ProjectVM(v)
	if err != nil {
		t.Fatalf("ProjectVM: %v", err)
	}
	if !strings.Contains(string(out), "network: net-a") {
		t.Errorf("want network: net-a, got:\n%s", out)
	}
	if strings.Contains(string(out), "net-b") {
		t.Errorf("second NIC must not be projected (single-NIC schema), got:\n%s", out)
	}
	docs, err := manifest.Parse(strings.NewReader(string(out)))
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if _, err := manifest.BuildCreatePlan(docs); err != nil {
		t.Fatalf("not apply-ready: %v", err)
	}
}
