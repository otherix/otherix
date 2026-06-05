// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestVMGetOutputYAML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"` + uuid.NewString() + `","name":"vm-mvp","owner_id":"` + uuid.NewString() +
			`","image_url":"https://img.example/noble.qcow2","format":"qcow2","pool":"default","node":null,` +
			`"networks":["net-mvp"],"architecture":"amd64","vcpus":2,"memory_mb":2048,"status":"running",` +
			`"desired_phase":"running","created_at":"2026-06-01T10:00:00Z","updated_at":"2026-06-01T10:00:00Z"}`))
	}))
	defer srv.Close()

	stdout, _, err := runVMCmd(t, srv.URL, []string{"get", "vm-mvp", "-o", "yaml"})
	if err != nil {
		t.Fatalf("get -o yaml error = %v", err)
	}
	for _, want := range []string{"apiVersion: otherix/v1", "kind: VM", "name: vm-mvp", "imageURL: https://img.example/noble.qcow2"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("yaml output missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "status") || strings.Contains(stdout, "id:") || strings.Contains(stdout, "owner") {
		t.Errorf("yaml output leaked server fields:\n%s", stdout)
	}
}
