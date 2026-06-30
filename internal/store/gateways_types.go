// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

// NodeKind discriminates the role a node row plays in the cluster. A node-kind
// row is a hypervisor that hosts VMs and is a candidate for VM placement; a
// gateway-kind row terminates ingress traffic and never hosts VMs, so the
// scheduler excludes it from placement. An empty Kind on an older row reads back
// as the node kind.
const (
	NodeKindNode    = "node"
	NodeKindGateway = "gateway"
)
