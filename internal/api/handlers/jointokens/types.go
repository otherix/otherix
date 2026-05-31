// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package jointokens

// createRequest is the body of POST /v1/nodes/join-tokens. Mirrors
// components/schemas/JoinTokenCreate in control-plane.yaml.
// IntendedNodeName uses *string so the absent / null cases are
// distinguishable from the empty-string case during validation;
// MaxUses uses *int32 with the same nil-vs-zero discipline (zero on
// the wire is a validation error — server rejects 0 explicitly).
type createRequest struct {
	Kind             *string `json:"kind"`
	IntendedNodeName *string `json:"intended_node_name"`
	TTLSeconds       *int    `json:"ttl_seconds"`
	MaxUses          *int32  `json:"max_uses"`
}

// joinTokenView is the public projection of a join_tokens row. The
// stored token_hash is NEVER part of this shape. consumption_count
// is a derived value (correlated subquery in ListJoinTokens / dedicated
// CountJoinTokenConsumptions on Get).
type joinTokenView struct {
	ID               string  `json:"id"`
	Kind             string  `json:"kind"`
	IntendedNodeName *string `json:"intended_node_name"`
	ExpiresAt        string  `json:"expires_at"`
	MaxUses          *int64  `json:"max_uses"`
	ConsumptionCount int64   `json:"consumption_count"`
	CreatedByUserID  *string `json:"created_by_user_id"`
	CreatedAt        string  `json:"created_at"`
}

// createResponse extends joinTokenView with the plaintext token and the
// active cluster CA fingerprint. The "token bundle" — operator
// copies the token + fingerprint to the agent host in one go.
type createResponse struct {
	joinTokenView
	Token               string `json:"token"`
	CAFingerprintSHA256 string `json:"ca_fingerprint_sha256"`
}

// listResponse is the cursor-paginated envelope.
type listResponse struct {
	Data []joinTokenView `json:"data"`
	Meta paginationMeta  `json:"meta"`
}

// consumptionView mirrors components/schemas/JoinTokenConsumption.
// source_ip stringifies to the netip.Addr canonical form.
type consumptionView struct {
	ID               string  `json:"id"`
	JoinTokenID      string  `json:"join_token_id"`
	ConsumedByNodeID *string `json:"consumed_by_node_id"`
	ConsumedAt       string  `json:"consumed_at"`
	SourceIP         *string `json:"source_ip"`
}

// consumptionsListResponse mirrors JoinTokenConsumptionList.
type consumptionsListResponse struct {
	Data []consumptionView `json:"data"`
	Meta paginationMeta    `json:"meta"`
}

// paginationMeta carries the opaque next_cursor; nil when current
// page is the last.
type paginationMeta struct {
	NextCursor *string `json:"next_cursor"`
}
