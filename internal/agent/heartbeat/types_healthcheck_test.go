// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestDeclaredHealthCheckJSONRoundTrip(t *testing.T) {
	in := DeclaredHealthCheck{
		VMID: uuid.New(), VMName: "web", LBID: uuid.New(), Port: 8080,
		IntervalSeconds: 5, TimeoutSeconds: 1, HealthyThreshold: 2, UnhealthyThreshold: 3,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var out DeclaredHealthCheck
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if out != in {
		t.Errorf("round trip = %+v, want %+v", out, in)
	}
}

func TestHealthCheckReportJSONTags(t *testing.T) {
	b, _ := json.Marshal(HealthCheckReport{LBID: uuid.Nil, VMID: uuid.Nil, Healthy: true})
	want := `{"lb_id":"00000000-0000-0000-0000-000000000000","vm_id":"00000000-0000-0000-0000-000000000000","healthy":true}`
	if string(b) != want {
		t.Errorf("HealthCheckReport JSON = %s, want %s", b, want)
	}
}
