// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import "encoding/json"

// Node role identifiers. hypervisor is a derived, VM-schedulable capability;
// gateway is the operator-assigned ingress role stored in Node.GatewayRole.
const (
	NodeRoleHypervisor = "hypervisor"
	NodeRoleGateway    = "gateway"
)

// EffectiveRoles returns the node's effective role set from its stored gateway
// bit and whether it owns any usable storage pool. hypervisor is derived from
// pool ownership (a node with a pool can host VMs); gateway is the stored,
// operator-assigned role. Order is [hypervisor, gateway]. A node that owns no
// pool and holds no gateway role has an empty, non-nil role set (so the wire
// field marshals to [] rather than null).
func EffectiveRoles(gatewayRole, ownsPool bool) []string {
	roles := make([]string, 0, 2)
	if ownsPool {
		roles = append(roles, NodeRoleHypervisor)
	}
	if gatewayRole {
		roles = append(roles, NodeRoleGateway)
	}
	return roles
}

// HasRole reports whether the node holds the given effective role. Only the
// stored gateway bit is answerable from the row alone; the hypervisor role is
// derived from storage-pool ownership at the read boundary (see EffectiveRoles),
// so HasRole never reports hypervisor. An unknown role is false.
func (n Node) HasRole(role string) bool {
	return role == NodeRoleGateway && n.GatewayRole
}

// UnmarshalJSON decodes a node row and migrates a pre-existing row that still
// carries the legacy Kind discriminator: when the row has no explicit
// GatewayRole and its Kind is the gateway kind, the gateway bit is set. A row
// written after this revision carries GatewayRole directly and is honored
// as-is. The migration is idempotent - re-encoding the decoded row persists
// GatewayRole.
func (n *Node) UnmarshalJSON(data []byte) error {
	type nodeAlias Node
	aux := struct {
		*nodeAlias
		Kind string
	}{nodeAlias: (*nodeAlias)(n)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	// The sibling Kind field captures the legacy "Kind" JSON key from a pre-roles
	// row so the gateway bit can be recovered; the row's own struct no longer
	// carries Kind.
	if !n.GatewayRole && aux.Kind == NodeKindGateway {
		n.GatewayRole = true
	}
	return nil
}
