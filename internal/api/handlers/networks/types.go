// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package networks

import "encoding/json"

// createRequest is the body of POST /v1/networks. Mirrors
// NetworkCreate in api/openapi/control-plane.yaml.
//
// vlan_tag is *int32 because the column is nullable; a request
// omitting the key is equivalent to passing null. mtu is *int32 so
// "field omitted" is distinguishable from "client explicitly sent 0"
// — the handler materialises a missing value to validation.DefaultMTU
// before insert. config is RawMessage so the handler can persist it
// verbatim into the jsonb column.
type createRequest struct {
	Name       string          `json:"name"`
	Type       string          `json:"type"`
	BridgeName string          `json:"bridge_name"`
	Managed    *bool           `json:"managed,omitempty"`
	Egress     *string         `json:"egress,omitempty"`
	VlanTag    *int32          `json:"vlan_tag,omitempty"`
	MTU        *int32          `json:"mtu,omitempty"`
	Subnet     *string         `json:"subnet,omitempty"`
	Gateway    *string         `json:"gateway,omitempty"`
	Dhcp       *bool           `json:"dhcp,omitempty"`
	Config     json.RawMessage `json:"config,omitempty"`
}

// updateRequest is the body of PATCH /v1/networks/{id}. Mirrors
// NetworkUpdate. Field-level semantics:
//
//   - Name / BridgeName: nil pointer = leave as-is, non-nil = set.
//   - MTU: nil pointer = leave as-is, non-nil = set (never null;
//     the column is NOT NULL since migration 00004).
//   - VlanTag: tri-state via plain (non-pointer) json.RawMessage —
//     length 0 means the key was absent; literal `null` clears the
//     tag; a JSON number sets it. *json.RawMessage cannot encode
//     this distinction because Go's json decoder collapses both
//     absence and null into a nil pointer.
//   - Config: same RawMessage shape as VlanTag, but `null` is
//     rejected as a validation error (the column is jsonb NOT NULL).
//   - Egress: nil pointer = leave as-is, non-nil = set ("none" or
//     "nat"). The resulting (type, managed, egress) triple is
//     re-validated after the merge.
//   - Subnet / Gateway: tri-state via plain json.RawMessage — length 0
//     means the key was absent (leave as-is); literal `null` clears the
//     value; a JSON string sets it. Clearing is only legal when the
//     resulting egress is none.
//
// `type`, `managed`, and `dhcp` are intentionally ABSENT from the struct.
// The handler does a pre-decode key sweep that rejects all three with 400
// forbidden_fields before this struct is decoded — `type` is API-immutable
// and `managed` / `dhcp` are fixed at creation time.
type updateRequest struct {
	Name       *string         `json:"name,omitempty"`
	BridgeName *string         `json:"bridge_name,omitempty"`
	Egress     *string         `json:"egress,omitempty"`
	VlanTag    json.RawMessage `json:"vlan_tag,omitempty"`
	MTU        *int32          `json:"mtu,omitempty"`
	Subnet     json.RawMessage `json:"subnet,omitempty"`
	Gateway    json.RawMessage `json:"gateway,omitempty"`
	Config     json.RawMessage `json:"config,omitempty"`
}

// listResponse is the payload for GET /v1/networks.
type listResponse struct {
	Data []networkView  `json:"data"`
	Meta paginationMeta `json:"meta"`
}

type paginationMeta struct {
	NextCursor *string `json:"next_cursor"`
}
