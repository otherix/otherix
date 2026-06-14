// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentapi

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMigrationIdentityFields locks in the two peer-identity fields the
// agent needs to pin its peer before the TLS handshake, and verifies their
// JSON behaviour: a set value survives a marshal/unmarshal round-trip, and
// because the fields are optional (pointer + omitempty) an unset value is
// omitted from the wire so older CP revisions that never send them still
// decode cleanly.
func TestMigrationIdentityFields(t *testing.T) {
	in := MigrationIncomingRequest{SourceNodeIdentity: ptr("CN=node-src")}
	inBytes, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal(MigrationIncomingRequest) error = %v", err)
	}
	var inBack MigrationIncomingRequest
	if err := json.Unmarshal(inBytes, &inBack); err != nil {
		t.Fatalf("Unmarshal(MigrationIncomingRequest) error = %v", err)
	}
	if inBack.SourceNodeIdentity == nil || *inBack.SourceNodeIdentity != "CN=node-src" {
		t.Errorf("SourceNodeIdentity round-trip = %v, want CN=node-src", inBack.SourceNodeIdentity)
	}

	out := MigrationOutgoingRequest{TargetNodeIdentity: ptr("node-tgt.agents.otherix.local")}
	outBytes, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("Marshal(MigrationOutgoingRequest) error = %v", err)
	}
	var outBack MigrationOutgoingRequest
	if err := json.Unmarshal(outBytes, &outBack); err != nil {
		t.Fatalf("Unmarshal(MigrationOutgoingRequest) error = %v", err)
	}
	if outBack.TargetNodeIdentity == nil || *outBack.TargetNodeIdentity != "node-tgt.agents.otherix.local" {
		t.Errorf("TargetNodeIdentity round-trip = %v, want node-tgt.agents.otherix.local", outBack.TargetNodeIdentity)
	}

	// Unset (nil) fields are omitted from the wire (omitempty), so a CP
	// that never sets them produces no key at all.
	zeroBytes, err := json.Marshal(MigrationIncomingRequest{})
	if err != nil {
		t.Fatalf("Marshal(zero MigrationIncomingRequest) error = %v", err)
	}
	if strings.Contains(string(zeroBytes), "source_node_identity") {
		t.Errorf("unset SourceNodeIdentity leaked into JSON: %s", zeroBytes)
	}
}

func ptr(s string) *string { return &s }
