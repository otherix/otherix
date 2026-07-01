// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// ingress-grant wire format: "otx_ingressgrant_" + base64url(32 random bytes).
// The longer prefix keeps grant tokens distinct from ordinary API
// tokens ("otx_") so the bearer dispatch can route a grant only to the
// ssh-cert and ssh-stream endpoints and never to any other route. The
// prefix is intentionally a superset of "otx_": callers that test for
// grant shape must check IsIngressGrantFormat before IsAPITokenFormat.
const (
	grantTokenPrefix = "otx_ingressgrant_" //nolint:gosec // G101: public routing prefix, not a credential; the secret is the random suffix.
	grantTokenBytes  = 32
)

// GenerateIngressGrantToken returns a fresh ingress-grant token: the plaintext to
// hand to the recipient once at creation and its storage hash. The 32
// random bytes come from crypto/rand, so each call yields a unique
// plaintext - the store's token-hash index is name-guarded only, so a
// reused plaintext would clobber another grant's index.
func GenerateIngressGrantToken() (plaintext string, hash []byte, err error) {
	buf := make([]byte, grantTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("generate grant token: %v", err)
	}
	plaintext = grantTokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	return plaintext, HashToken(plaintext), nil
}

// IsIngressGrantFormat reports whether s carries the ingress-grant prefix.
// The bearer dispatch uses this to route grant tokens to the ssh-cert
// and ssh-stream endpoints. It checks shape only, not validity.
func IsIngressGrantFormat(s string) bool {
	return strings.HasPrefix(s, grantTokenPrefix)
}

// GrantVMScope is what a grant authorizes on one VM: the guest TCP ports the
// bearer may reach, plus the optional pinned SSH login consulted when the
// reached port is the SSH port.
type GrantVMScope struct {
	Ports []int
	Login string
}

// GrantPrincipal is the synthetic principal an ingress-grant token resolves
// to at connect time. It is deliberately not an auth.User and carries
// exactly one capability, vm:ssh, scoped to the grant's current VM set;
// every other capability is denied. VMs maps vm_name to the ports and pinned
// login authorized on that VM. Resolution happens against the freshly loaded
// grant, so a revoke or scope shrink takes effect immediately.
type GrantPrincipal struct {
	GrantID   uuid.UUID
	VMs       map[string]GrantVMScope
	ExpiresAt *time.Time
	Revoked   bool
}

// GrantPrincipalFromStore builds a GrantPrincipal from a stored grant,
// flattening its per-VM ports and logins into the vm_name -> scope map.
func GrantPrincipalFromStore(g store.IngressGrant) GrantPrincipal {
	vms := make(map[string]GrantVMScope, len(g.VMs))
	for _, vm := range g.VMs {
		vms[vm.VMName] = GrantVMScope{Ports: vm.Ports, Login: vm.Login}
	}
	return GrantPrincipal{
		GrantID:   g.ID,
		VMs:       vms,
		ExpiresAt: g.ExpiresAt,
		Revoked:   g.Revoked,
	}
}

// CanReach reports whether the grant authorizes vmName on port at time now and,
// if so, the pinned login. It returns false when the grant is revoked, has
// expired, vmName is not in the set, or port is not in that VM's port set.
func (p GrantPrincipal) CanReach(vmName string, port int, now time.Time) (login string, ok bool) {
	if p.Revoked {
		return "", false
	}
	if p.ExpiresAt != nil && !now.Before(*p.ExpiresAt) {
		return "", false
	}
	scope, ok := p.VMs[vmName]
	if !ok {
		return "", false
	}
	for _, allowed := range scope.Ports {
		if allowed == port {
			return scope.Login, true
		}
	}
	return "", false
}
