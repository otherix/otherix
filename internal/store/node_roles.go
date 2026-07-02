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

// NodeRoles returns the effective role set for a node given its stored gateway
// bit. Exactly one role per node in this revision: a gateway node reports
// [gateway]; every other node reports the derived [hypervisor].
func NodeRoles(gatewayRole bool) []string {
	if gatewayRole {
		return []string{NodeRoleGateway}
	}
	return []string{NodeRoleHypervisor}
}

// HasRole reports whether the node holds the given effective role. gateway is
// the stored bit; hypervisor is derived as its inverse. An unknown role is
// false.
func (n Node) HasRole(role string) bool {
	switch role {
	case NodeRoleGateway:
		return n.GatewayRole
	case NodeRoleHypervisor:
		return !n.GatewayRole
	default:
		return false
	}
}

// Roles returns the node's effective role set.
func (n Node) Roles() []string { return NodeRoles(n.GatewayRole) }

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
	// The sibling Kind field shadows the embedded alias's Kind for the "Kind"
	// JSON key, so restore it onto the node; Kind is still a live field.
	n.Kind = aux.Kind
	if !n.GatewayRole && aux.Kind == NodeKindGateway {
		n.GatewayRole = true
	}
	return nil
}
