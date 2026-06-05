// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package network_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestNetworkGetOutputYAML(t *testing.T) {
	id := uuid.NewString()
	obj := `{"id":"` + id + `","name":"net-dev","type":"bridge","bridge_name":"br0","managed":false,"egress":"none","mtu":1500}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// `get net-dev` resolves the name via the list route, then fetches by id.
		if r.URL.Path == "/v1/networks" {
			_, _ = w.Write([]byte(`{"data":[` + obj + `],"meta":{"next_cursor":null}}`))
			return
		}
		_, _ = w.Write([]byte(obj))
	}))
	defer srv.Close()

	stdout, _, err := runNetworkCmd(t, srv.URL, []string{"get", "net-dev", "-o", "yaml"})
	if err != nil {
		t.Fatalf("get -o yaml error = %v", err)
	}
	for _, want := range []string{"apiVersion: otherix/v1", "kind: Network", "name: net-dev", "bridgeName: br0"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("yaml output missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "status") || strings.Contains(stdout, "id:") {
		t.Errorf("yaml output leaked server fields:\n%s", stdout)
	}
}

func TestNetworkListOutputYAML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[` +
			`{"id":"` + uuid.NewString() + `","name":"net-a","type":"bridge","bridge_name":"br0","managed":false,"egress":"none","mtu":1500},` +
			`{"id":"` + uuid.NewString() + `","name":"net-b","type":"bridge","bridge_name":"br1","managed":false,"egress":"none","mtu":1500}` +
			`],"meta":{"next_cursor":null}}`))
	}))
	defer srv.Close()

	stdout, _, err := runNetworkCmd(t, srv.URL, []string{"list", "-o", "yaml"})
	if err != nil {
		t.Fatalf("list -o yaml error = %v", err)
	}
	if got := strings.Count(stdout, "kind: Network"); got != 2 {
		t.Errorf("kind: Network count = %d, want 2:\n%s", got, stdout)
	}
	if !strings.Contains(stdout, "name: net-a") || !strings.Contains(stdout, "name: net-b") {
		t.Errorf("list yaml missing both networks:\n%s", stdout)
	}
	if got := strings.Count(stdout, "---"); got != 1 {
		t.Errorf("document separator count = %d, want exactly 1 between 2 docs:\n%s", got, stdout)
	}
}
