// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// grantVMForID builds a stored grant authorizing vmName on port 22, bound to
// boundID (pass uuid.Nil to model a legacy name-only grant).
func grantVMForID(vmName string, boundID uuid.UUID) store.IngressGrant {
	return store.IngressGrant{
		ID:  uuid.New(),
		VMs: []store.IngressGrantVM{{VMName: vmName, VMID: boundID, Ports: []int{22}, Login: "ubuntu"}},
	}
}

// TestAuthorizeIngressGrant_NameReuseVMIDMismatchRejected: on the broker/gateway
// path, a grant bound to VM "demo" at UUID X must not authorize the broker once
// the name "demo" resolves to a DIFFERENT VM (UUID Y) - the deleted-and-reused
// name case. The connect-time VM-ID assertion rejects the stale binding.
func TestAuthorizeIngressGrant_NameReuseVMIDMismatchRejected(t *testing.T) {
	t.Parallel()
	boundID := uuid.New()
	reusedID := uuid.New()
	st := &relayStoreStub{
		grant: grantVMForID("demo", boundID),
		vm:    store.VM{ID: reusedID, Name: "demo"},
	}
	h := relayHandler(st, &dialSpyClient{})

	if _, ok := h.authorizeIngressGrant(relayRequest("demo", "otx_ingressgrant_abc"), "otx_ingressgrant_abc", "demo", 22); ok {
		t.Errorf("authorizeIngressGrant on VM-ID mismatch = ok, want reject")
	}
}

// TestAuthorizeIngressGrant_VMIDMatchAuthorizes is the positive counterpart: a
// grant whose bound VMID equals the name-resolved VM's id authorizes the broker
// and returns that VM, proving the assertion is not over-broad.
func TestAuthorizeIngressGrant_VMIDMatchAuthorizes(t *testing.T) {
	t.Parallel()
	vmID := uuid.New()
	st := &relayStoreStub{
		grant: grantVMForID("demo", vmID),
		vm:    store.VM{ID: vmID, Name: "demo"},
	}
	h := relayHandler(st, &dialSpyClient{})

	vm, ok := h.authorizeIngressGrant(relayRequest("demo", "otx_ingressgrant_abc"), "otx_ingressgrant_abc", "demo", 22)
	if !ok {
		t.Fatalf("authorizeIngressGrant on VM-ID match = reject, want ok")
	}
	if vm.ID != vmID {
		t.Errorf("returned vm.ID = %v, want %v", vm.ID, vmID)
	}
}

// TestAuthorizeIngressGrant_LegacyNilVMIDAuthorizes: a legacy grant carrying a
// zero VMID (minted before the id binding existed) is treated as name-only, so
// the broker authorizes on the name match alone - preserving backward compat.
func TestAuthorizeIngressGrant_LegacyNilVMIDAuthorizes(t *testing.T) {
	t.Parallel()
	st := &relayStoreStub{
		grant: grantVMForID("demo", uuid.Nil),
		vm:    store.VM{ID: uuid.New(), Name: "demo"},
	}
	h := relayHandler(st, &dialSpyClient{})

	if _, ok := h.authorizeIngressGrant(relayRequest("demo", "otx_ingressgrant_abc"), "otx_ingressgrant_abc", "demo", 22); !ok {
		t.Errorf("authorizeIngressGrant on legacy nil VMID = reject, want ok (name-only)")
	}
}
