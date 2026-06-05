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
	obj := `{"id":"` + id + `","name":"net-mvp","type":"bridge","bridge_name":"br0","managed":false,"egress":"none","mtu":1500}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// `get net-mvp` resolves the name via the list route, then fetches by id.
		if r.URL.Path == "/v1/networks" {
			_, _ = w.Write([]byte(`{"data":[` + obj + `],"meta":{"next_cursor":null}}`))
			return
		}
		_, _ = w.Write([]byte(obj))
	}))
	defer srv.Close()

	stdout, _, err := runNetworkCmd(t, srv.URL, []string{"get", "net-mvp", "-o", "yaml"})
	if err != nil {
		t.Fatalf("get -o yaml error = %v", err)
	}
	for _, want := range []string{"apiVersion: otherix/v1", "kind: Network", "name: net-mvp", "bridgeName: br0"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("yaml output missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "status") || strings.Contains(stdout, "id:") {
		t.Errorf("yaml output leaked server fields:\n%s", stdout)
	}
}
