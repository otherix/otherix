// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package manifest_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"gopkg.in/yaml.v3"

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
	n := cpclient.Network{Name: "ovl", Type: "overlay", BridgeName: "otvb100", MTU: 1390, Subnet: &subnet}
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

func TestProjectNetworkOverlayRoundTripsNatEgress(t *testing.T) {
	// egress is user-set for an overlay (none|nat), NOT server-derived, so a nat
	// overlay must round-trip egress: nat - otherwise re-applying silently
	// downgrades it to egress: none and drops the NAT.
	subnet := "10.62.0.0/24"
	n := cpclient.Network{Name: "ovl-nat", Type: "overlay", Egress: "nat", Subnet: &subnet}
	out, err := manifest.ProjectNetwork(n)
	if err != nil {
		t.Fatalf("ProjectNetwork() error = %v", err)
	}
	if !strings.Contains(string(out), "egress: nat") {
		t.Errorf("overlay projection missing egress: nat:\n%s", out)
	}
	docs, err := manifest.Parse(strings.NewReader(string(out)))
	if err != nil {
		t.Fatalf("re-parse overlay projection: %v", err)
	}
	plan, err := manifest.BuildCreatePlan(docs)
	if err != nil {
		t.Fatalf("overlay projection not apply-ready: %v", err)
	}
	if len(plan) != 1 || plan[0].Network == nil {
		t.Fatalf("plan = %+v, want 1 network op", plan)
	}
	if plan[0].Network.Egress != "nat" {
		t.Errorf("Network.Egress = %q, want nat after round-trip", plan[0].Network.Egress)
	}
}

func TestProjectNetworkRoundTripsDhcp(t *testing.T) {
	// dhcp is emitted only when the live network's Dhcp pointer is non-nil
	// and true, mirroring the conditional egress projection. A dhcp overlay
	// network must round-trip the flag through `get -o yaml | create -f`.
	dhcp := true
	subnet := "10.50.0.0/24"
	n := cpclient.Network{Name: "net-dhcp", Type: "overlay", Egress: "nat", Subnet: &subnet, Dhcp: &dhcp}
	out, err := manifest.ProjectNetwork(n)
	if err != nil {
		t.Fatalf("ProjectNetwork() error = %v", err)
	}
	if !strings.Contains(string(out), "dhcp: true") {
		t.Errorf("projection missing dhcp: true:\n%s", out)
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
	if !plan[0].Network.Dhcp {
		t.Errorf("Network.Dhcp = false, want true after round-trip")
	}
}

func TestProjectNetworkOmitsDhcpWhenFalseOrNil(t *testing.T) {
	// A nil or false Dhcp pointer must not emit a dhcp line, matching the
	// non-default-only projection of egress/subnet/gateway.
	n := cpclient.Network{Name: "net-dev", Type: "bridge", BridgeName: "br0"}
	out, err := manifest.ProjectNetwork(n)
	if err != nil {
		t.Fatalf("ProjectNetwork() error = %v", err)
	}
	if strings.Contains(string(out), "dhcp") {
		t.Errorf("projection must omit dhcp when nil:\n%s", out)
	}
	dhcpFalse := false
	n2 := cpclient.Network{Name: "net-dev2", Type: "bridge", BridgeName: "br0", Dhcp: &dhcpFalse}
	out2, err := manifest.ProjectNetwork(n2)
	if err != nil {
		t.Fatalf("ProjectNetwork() error = %v", err)
	}
	if strings.Contains(string(out2), "dhcp") {
		t.Errorf("projection must omit dhcp when false:\n%s", out2)
	}
}

func TestProjectNetworkRoundTripsDNS(t *testing.T) {
	// dns is emitted only when it differs from the type's default (bridge: dns
	// follows dhcp; overlay: dns defaults true). A managed bridge with dhcp on
	// but dns off must round-trip dns: false through get -o yaml | create -f.
	subnet := "10.20.0.0/24"
	dhcp, dnsOff := true, false
	n := cpclient.Network{
		Name: "net-app", Type: "bridge", BridgeName: "br-app", Managed: true,
		Egress: "nat", Subnet: &subnet, Dhcp: &dhcp, DNS: &dnsOff, MTU: 1500,
	}
	out, err := manifest.ProjectNetwork(n)
	if err != nil {
		t.Fatalf("ProjectNetwork() error = %v", err)
	}
	if !strings.Contains(string(out), "dns: false") {
		t.Errorf("projection missing dns: false:\n%s", out)
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
	if plan[0].Network.DNS == nil || *plan[0].Network.DNS != false {
		t.Errorf("Network.DNS = %v, want non-nil false after round-trip", plan[0].Network.DNS)
	}
}

func TestProjectNetworkOmitsDNSWhenDefault(t *testing.T) {
	// dns defaults to the dhcp value for BOTH types, so a dns line is omitted
	// whenever dns equals dhcp: a bridge or overlay with dhcp=true,dns=true, a
	// plain overlay with dhcp=false,dns=false, and any view that did not surface
	// dns.
	subnet := "10.20.0.0/24"
	dhcpOn, dnsOn := true, true
	dhcpOff, dnsOff := false, false
	cases := []cpclient.Network{
		{Name: "b", Type: "bridge", BridgeName: "br0", Managed: true, Egress: "nat", Subnet: &subnet, Dhcp: &dhcpOn, DNS: &dnsOn, MTU: 1500},
		{Name: "o", Type: "overlay", Egress: "nat", Subnet: &subnet, Dhcp: &dhcpOn, DNS: &dnsOn},
		{Name: "p", Type: "overlay", Subnet: &subnet, Dhcp: &dhcpOff, DNS: &dnsOff}, // plain overlay: dhcp=false,dns=false
		{Name: "n", Type: "bridge", BridgeName: "br0"},                              // DNS nil: not surfaced
	}
	for _, n := range cases {
		out, err := manifest.ProjectNetwork(n)
		if err != nil {
			t.Fatalf("ProjectNetwork(%s) error = %v", n.Name, err)
		}
		if strings.Contains(string(out), "dns") {
			t.Errorf("projection for %s must omit dns at default:\n%s", n.Name, out)
		}
	}
}

func TestProjectNetworkOverlayRoundTripsDivergentDNS(t *testing.T) {
	// dns defaults to the dhcp value for an overlay (same as a bridge), so dns is
	// emitted only when it diverges from dhcp: a resolver-only overlay
	// (dhcp=false, dns=true) round-trips dns: true.
	subnet := "10.62.0.0/24"
	dhcpOff, dnsOn := false, true
	n := cpclient.Network{Name: "ovl-dns", Type: "overlay", Subnet: &subnet, Dhcp: &dhcpOff, DNS: &dnsOn}
	out, err := manifest.ProjectNetwork(n)
	if err != nil {
		t.Fatalf("ProjectNetwork() error = %v", err)
	}
	if !strings.Contains(string(out), "dns: true") {
		t.Errorf("overlay projection missing dns: true (diverges from dhcp=false default):\n%s", out)
	}
	docs, err := manifest.Parse(strings.NewReader(string(out)))
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	plan, err := manifest.BuildCreatePlan(docs)
	if err != nil {
		t.Fatalf("not apply-ready: %v", err)
	}
	if len(plan) != 1 || plan[0].Network == nil || plan[0].Network.DNS == nil || *plan[0].Network.DNS != true {
		t.Errorf("Network.DNS did not round-trip to non-nil true: %+v", plan)
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

// TestProjectVMRoundTripsLabels guards that a labeled VM round-trips through
// `vm get -o yaml | create -f`: ProjectVM emits spec.labels, and the re-parsed
// create plan carries the same labels into the create request (the field load
// balancers select backends by).
func TestProjectVMRoundTripsLabels(t *testing.T) {
	v := cpclient.VM{
		Name:         "vm-labeled",
		ImageURL:     "http://x/i.qcow2",
		Architecture: "amd64",
		VCPUs:        2,
		MemoryMB:     1024,
		Labels:       map[string]any{"app": "web", "tier": "frontend"},
	}
	out, err := manifest.ProjectVM(v)
	if err != nil {
		t.Fatalf("ProjectVM() error = %v", err)
	}
	docs, err := manifest.Parse(strings.NewReader(string(out)))
	if err != nil {
		t.Fatalf("re-parse projected VM: %v\n%s", err, out)
	}
	plan, err := manifest.BuildCreatePlan(docs)
	if err != nil {
		t.Fatalf("projected VM is not apply-ready: %v", err)
	}
	if len(plan) != 1 || plan[0].VM == nil {
		t.Fatalf("BuildCreatePlan = %+v, want one VM op", plan)
	}
	got := plan[0].VM.Labels
	if got["app"] != "web" || got["tier"] != "frontend" || len(got) != 2 {
		t.Errorf("round-tripped labels = %v, want {app:web, tier:frontend}", got)
	}
}

// TestProjectVM_StatusAndSourceSnapshot guards that `vm get -o yaml`
// surfaces the two pieces the server already returns but the projection
// historically dropped: the system-reported status (top-level, k8s-style,
// sibling of spec) and the source_snapshot_id of a snapshot-restored VM
// (inside spec, a create input). status must NOT land inside spec (it is
// observed state, not a create input), and sourceSnapshotID must.
func TestProjectVM_StatusAndSourceSnapshot(t *testing.T) {
	snapID := "11111111-2222-3333-4444-555555555555"
	v := cpclient.VM{
		Name:             "vm-from-snap",
		Architecture:     "amd64",
		VCPUs:            2,
		MemoryMB:         2048,
		SourceSnapshotID: &snapID,
		Status:           cpclient.VMStatus{Phase: "running"},
	}
	out, err := manifest.ProjectVM(v)
	if err != nil {
		t.Fatalf("ProjectVM() error = %v", err)
	}
	got := string(out)

	// Decode the projection to inspect structure precisely (top-level
	// status, sourceSnapshotID under spec).
	var doc struct {
		Status struct {
			Phase string `yaml:"phase"`
		} `yaml:"status"`
		Spec struct {
			SourceSnapshotID string `yaml:"sourceSnapshotID"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal projection: %v\n%s", err, got)
	}
	if doc.Status.Phase != "running" {
		t.Errorf("top-level status.phase = %q, want running\n%s", doc.Status.Phase, got)
	}
	if doc.Spec.SourceSnapshotID != snapID {
		t.Errorf("spec.sourceSnapshotID = %q, want %q\n%s", doc.Spec.SourceSnapshotID, snapID, got)
	}
	// status must be a sibling of spec, never nested inside it: a spec
	// block carrying status would feed observed state into a create input.
	// A top-level status key sits at column 0; a nested one would be
	// indented. Decoding into a typed doc also independently confirms the
	// placement (doc.Spec has no status field), but assert on the text too
	// so a regression to nesting is caught directly.
	if !strings.Contains(got, "\nstatus:\n") {
		t.Errorf("status must be a top-level key (column 0):\n%s", got)
	}
}

// TestVMManifestWithStatusStillApplies confirms a projected VM manifest
// carrying a top-level status block is still apply-ready: the create/apply
// path (Parse -> BuildCreatePlan) reads spec only and ignores status, so a
// `vm get -o yaml | create -f` round-trip does not break on the status block.
func TestVMManifestWithStatusStillApplies(t *testing.T) {
	v := cpclient.VM{
		Name: "vm-applied", ImageURL: "http://x/i.qcow2", Architecture: "amd64",
		VCPUs: 2, MemoryMB: 1024, Status: cpclient.VMStatus{Phase: "running"},
	}
	out, err := manifest.ProjectVM(v)
	if err != nil {
		t.Fatalf("ProjectVM() error = %v", err)
	}
	if !strings.Contains(string(out), "status:") {
		t.Fatalf("expected a status block in the projection:\n%s", out)
	}
	docs, err := manifest.Parse(strings.NewReader(string(out)))
	if err != nil {
		t.Fatalf("re-parse projection with status: %v\n%s", err, out)
	}
	plan, err := manifest.BuildCreatePlan(docs)
	if err != nil {
		t.Fatalf("manifest with status is not apply-ready: %v\n%s", err, out)
	}
	if len(plan) != 1 || plan[0].VM == nil {
		t.Fatalf("plan = %+v, want 1 vm op", plan)
	}
}

// TestProjectVMOmitsStatusWhenPhaseEmpty guards that a VM with no reported
// phase does not emit a noisy empty status block.
func TestProjectVMOmitsStatusWhenPhaseEmpty(t *testing.T) {
	v := cpclient.VM{Name: "vm1", ImageURL: "http://x/i.qcow2", Architecture: "amd64", VCPUs: 1, MemoryMB: 512}
	out, err := manifest.ProjectVM(v)
	if err != nil {
		t.Fatalf("ProjectVM() error = %v", err)
	}
	if strings.Contains(string(out), "status:") {
		t.Errorf("projection must omit status when phase is empty:\n%s", out)
	}
	if strings.Contains(string(out), "sourceSnapshotID") {
		t.Errorf("projection must omit sourceSnapshotID when unset:\n%s", out)
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
// round-trip. (firmware/firmwareID/diskGiB are intentionally absent from
// the view and so cannot round-trip - see the docs caveat.) Cloud-init
// payloads (userData/networkConfig) DO surface on the view and must
// round-trip.
func TestProjectVMRoundTripPreservesViewFields(t *testing.T) {
	node := "node-7"
	userData := "#cloud-config\npackages: [htop]\n"
	netCfg := "version: 2\nethernets: {}\n"
	v := cpclient.VM{
		Name: "vm-full", ImageURL: "http://x/i.qcow2", ImageSHA256: "abc123",
		Architecture: "arm64", Format: "raw", Pool: "pool-a", Node: &node,
		Networks: []string{"net-a"}, VCPUs: 8, MemoryMB: 16384,
		UserData: &userData, NetworkConfig: &netCfg,
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
	if got.UserData == nil || *got.UserData != userData {
		t.Errorf("userData = %v, want %q", got.UserData, userData)
	}
	if got.NetworkConfig == nil || *got.NetworkConfig != netCfg {
		t.Errorf("networkConfig = %v, want %q", got.NetworkConfig, netCfg)
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

// TestProjectLoadBalancerRoundTrips proves a `lb get -o yaml` projection
// re-parses to an equivalent create plan: port and selector survive
// `get -o yaml | create -f`, and the projection carries no server-assigned
// identity fields.
func TestProjectLoadBalancerRoundTrips(t *testing.T) {
	lb := cpclient.LoadBalancer{
		ID:       "22222222-3333-4444-5555-666666666666",
		Name:     "web",
		OwnerID:  "99999999-9999-9999-9999-999999999999",
		Port:     8080,
		Selector: map[string]string{"app": "web", "tier": "fe"},
	}
	out, err := manifest.ProjectLoadBalancer(lb)
	if err != nil {
		t.Fatalf("ProjectLoadBalancer() error = %v", err)
	}
	if strings.Contains(string(out), "id:") || strings.Contains(string(out), "owner") {
		t.Errorf("projection leaked server fields:\n%s", out)
	}
	plan := mustCreatePlan(t, out)
	if len(plan) != 1 || plan[0].LB == nil {
		t.Fatalf("plan = %+v, want 1 load balancer op", plan)
	}
	got := plan[0].LB
	if got.Name != "web" {
		t.Errorf("name = %q, want web", got.Name)
	}
	if got.Port != 8080 {
		t.Errorf("port = %d, want 8080", got.Port)
	}
	if diff := cmp.Diff(map[string]string{"app": "web", "tier": "fe"}, got.Selector); diff != "" {
		t.Errorf("selector mismatch (-want +got):\n%s", diff)
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
