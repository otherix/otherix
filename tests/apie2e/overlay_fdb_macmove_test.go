// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"context"
	"encoding/json"
	"net"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// fdbHeartbeatResponse decodes the declared_fdb slice from the heartbeat
// response. declaredFDBEntry mirrors the CP-side wire shape: {vni, mac, vtep_ip},
// where the all-zeros mac is the BUM/flood entry.
type fdbHeartbeatResponse struct {
	DeclaredFDB []fdbEntry `json:"declared_fdb"`
}

type fdbEntry struct {
	VNI    int32  `json:"vni"`
	MAC    string `json:"mac"`
	VtepIP string `json:"vtep_ip"`
}

// fdbHasUnicast reports whether the declared_fdb slice carries a unicast entry
// for the exact MAC mac pointing at vtep. It ignores the all-zeros flood entry,
// which shares the same VTEP but carries the zero MAC.
func fdbHasUnicast(fdb []fdbEntry, mac, vtep string) bool {
	for _, e := range fdb {
		if e.MAC == mac && e.VtepIP == vtep {
			return true
		}
	}
	return false
}

// fdbVtepForMAC returns the VTEP the unicast entry for mac points at, or "" when
// the slice has no unicast entry for mac. Used for clearer failure messages.
func fdbVtepForMAC(fdb []fdbEntry, mac string) string {
	for _, e := range fdb {
		if e.MAC == mac {
			return e.VtepIP
		}
	}
	return ""
}

// seedOverlayPlacement writes the two keys the overlay placement projection
// reads for one NIC - the vm_nic row plus its per-network index entry - and the
// vm_runtime row pinning the owning VM to nodeID. It mirrors the store-level
// seed helpers (seedVMWithNIC + placeVM) but drives the apie2e shared client so
// the apie2e harness can stage an overlay placement without the full
// CreateScheduledVM path. Returns the owning VM id so the caller can re-pin the
// runtime to a different node (the MAC move).
func seedOverlayPlacement(t *testing.T, networkID uuid.UUID, mac net.HardwareAddr, nodeID uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	cli := sharedEtcdClient
	vmID := uuid.New()
	nicID := uuid.New()
	nic := store.VMNic{
		ID:         nicID,
		VmID:       vmID,
		NetworkID:  networkID,
		Model:      store.NicModelVirtio,
		MacAddress: mac,
		Generation: 1,
	}
	if err := cli.PutJSON(ctx, etcd.Key("vm_nics", nicID.String()), nic); err != nil {
		t.Fatalf("seedOverlayPlacement: put nic: %v", err)
	}
	idxKey := etcd.Key("index", "vm_nics", "network", networkID.String(), nicID.String())
	if err := cli.Put(ctx, idxKey, []byte(nicID.String())); err != nil {
		t.Fatalf("seedOverlayPlacement: put network index: %v", err)
	}
	repinRuntime(t, vmID, nodeID)
	return vmID
}

// repinRuntime writes the vm_runtime row pinning vmID to nodeID - the single
// runtime field the overlay placement projection joins on. Re-invoking it with a
// new node is the MAC move: the NIC (and its MAC) is untouched; only the owning
// VM's current node changes.
func repinRuntime(t *testing.T, vmID, nodeID uuid.UUID) {
	t.Helper()
	rt := store.VMRuntime{VmID: vmID, CurrentNodeID: &nodeID, Phase: store.VmPhaseRunning}
	if err := sharedEtcdClient.PutJSON(context.Background(), etcd.Key("vm_runtime", vmID.String()), rt); err != nil {
		t.Fatalf("repinRuntime: %v", err)
	}
}

// mustParseMAC parses a MAC literal or fails the test.
func mustParseMAC(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	mac, err := net.ParseMAC(s)
	if err != nil {
		t.Fatalf("ParseMAC(%q): %v", s, err)
	}
	return mac
}

// fdbHeartbeat drives ag's heartbeat over the real mTLS agent router and decodes
// the declared_fdb slice. It piggybacks on wgSendHeartbeat's request shape but
// re-decodes the body for the FDB fields.
func fdbHeartbeat(t *testing.T, baseURL string, ag wgAgent, rep *wgHeartbeatReport) []fdbEntry {
	t.Helper()
	// wgSendHeartbeat already asserts a 200 and consumes the body for the WG
	// fields; we need the FDB fields off the SAME response, so issue our own
	// request via wgSendHeartbeatStatus and decode the raw body here.
	status, raw := wgSendHeartbeatStatus(t, baseURL, ag, rep)
	if status != 200 {
		t.Fatalf("heartbeat status = %d, want 200; body=%s", status, raw)
	}
	var out fdbHeartbeatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode declared_fdb: %v; body=%s", err, raw)
	}
	return out.DeclaredFDB
}

// TestOverlayFDBProjectionAcrossMACMove pins the CP-side overlay FDB projection
// across a cross-node MAC move, driven through the REAL heartbeat handler over a
// REAL store. A VM NIC with MAC M starts placed on node A; a third observer node
// C (which itself has a local NIC on the same overlay, so it participates in the
// VNI and the projection emits head-end-replication entries to it) must see a
// unicast (M, vtep_A) entry. After the NIC's owning VM is re-pinned from A to B,
// C's projection must drop (M, vtep_A) and emit (M, vtep_B) - no stale entry may
// linger. This is the real-path complement to the agent-side op-log test and the
// store-level placement snapshot test.
func TestOverlayFDBProjectionAcrossMACMove(t *testing.T) {
	h := newE2E(t)

	// Overlay network (forces VNI allocation). Created through the store so we
	// capture the CP-assigned VNI the FDB entries carry.
	ov, err := h.store.CreateNetwork(context.Background(), store.CreateNetworkParams{
		ID:     uuid.New(),
		Name:   "ov-fdb-macmove-" + uuid.NewString()[:8],
		Type:   store.NetworkTypeOverlay,
		Config: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateNetwork(overlay): %v", err)
	}
	if ov.VNI == nil {
		t.Fatalf("overlay network has nil VNI")
	}
	vni := *ov.VNI

	// Three agents on the mTLS agent router. Each reports WG state once so the CP
	// allocates its VTEP/overlay IP: A->10.42.0.1, B->10.42.0.2, C->10.42.0.3
	// (allocation order, default /16 supernet).
	caCert, caKey := wgGenerateCA(t)
	agentSrv := wgStartAgentTLSServer(t, h, caCert, caKey)

	a := wgSeedAgent(t, h, caCert, caKey, "node-a")
	b := wgSeedAgent(t, h, caCert, caKey, "node-b")
	c := wgSeedAgent(t, h, caCert, caKey, "node-c")

	wgSendHeartbeat(t, agentSrv.URL, a, &wgHeartbeatReport{PublicKey: "pkA", Endpoint: "a.example:51820", ListenPort: 51820})
	wgSendHeartbeat(t, agentSrv.URL, b, &wgHeartbeatReport{PublicKey: "pkB", Endpoint: "b.example:51820", ListenPort: 51820})
	wgSendHeartbeat(t, agentSrv.URL, c, &wgHeartbeatReport{PublicKey: "pkC", Endpoint: "c.example:51820", ListenPort: 51820})

	const (
		vtepA = "10.42.0.1"
		vtepB = "10.42.0.2"
	)

	// The roaming NIC: MAC M, owned by a VM placed on A.
	const macM = "52:54:00:00:00:0a"
	macMoveVM := seedOverlayPlacement(t, ov.ID, mustParseMAC(t, macM), a.nodeID)

	// C only emits FDB for overlays it participates in LOCALLY (head-end
	// replication is for REMOTE MACs on a VNI the node itself is on). Give C its
	// own local NIC on the overlay so the projection emits the remote (M) entry.
	seedOverlayPlacement(t, ov.ID, mustParseMAC(t, "52:54:00:00:00:0c"), c.nodeID)

	// First observation: C must see M at A's VTEP, not B's.
	fdb1 := fdbHeartbeat(t, agentSrv.URL, c, &wgHeartbeatReport{PublicKey: "pkC", Endpoint: "c.example:51820", ListenPort: 51820})
	if !fdbHasUnicast(fdb1, macM, vtepA) {
		t.Errorf("before move: C declared_fdb missing unicast (%s, %s); got vtep=%q for M; fdb=%+v",
			macM, vtepA, fdbVtepForMAC(fdb1, macM), fdb1)
	}
	if fdbHasUnicast(fdb1, macM, vtepB) {
		t.Errorf("before move: C declared_fdb has premature unicast (%s, %s); fdb=%+v", macM, vtepB, fdb1)
	}
	// Sanity: the VNI on M's entry matches the overlay's.
	for _, e := range fdb1 {
		if e.MAC == macM && e.VNI != vni {
			t.Errorf("before move: M entry vni = %d, want %d", e.VNI, vni)
		}
	}

	// The MAC move: re-pin M's owning VM from A to B. The NIC (and its MAC) is
	// unchanged; only vm_runtime.current_node_id flips - the placement source the
	// projection reads.
	repinRuntime(t, macMoveVM, b.nodeID)

	// Second observation: C must now see M at B's VTEP, with the stale A entry
	// gone.
	fdb2 := fdbHeartbeat(t, agentSrv.URL, c, &wgHeartbeatReport{PublicKey: "pkC", Endpoint: "c.example:51820", ListenPort: 51820})
	if !fdbHasUnicast(fdb2, macM, vtepB) {
		t.Errorf("after move: C declared_fdb missing unicast (%s, %s); got vtep=%q for M; fdb=%+v",
			macM, vtepB, fdbVtepForMAC(fdb2, macM), fdb2)
	}
	if fdbHasUnicast(fdb2, macM, vtepA) {
		t.Errorf("after move: C declared_fdb still has STALE unicast (%s, %s); the move was not reflected; fdb=%+v",
			macM, vtepA, fdb2)
	}
}
