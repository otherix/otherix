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
		_, _ = w.Write([]byte(`{"id":"` + uuid.NewString() + `","name":"vm-dev","owner_id":"` + uuid.NewString() +
			`","image_url":"https://img.example/noble.qcow2","format":"qcow2","pool":"default","node":null,` +
			`"networks":["net-dev"],"architecture":"amd64","vcpus":2,"memory_mib":2048,"status":{"phase":"running"},` +
			`"desired_phase":"running","created_at":"2026-06-01T10:00:00Z","updated_at":"2026-06-01T10:00:00Z"}`))
	}))
	defer srv.Close()

	stdout, _, err := runVMCmd(t, srv.URL, []string{"get", "vm-dev", "-o", "yaml"})
	if err != nil {
		t.Fatalf("get -o yaml error = %v", err)
	}
	for _, want := range []string{
		"apiVersion: otherix/v1", "kind: VM", "name: vm-dev",
		"imageURL: https://img.example/noble.qcow2",
		// status is observed state surfaced top-level (sibling of spec) so the
		// operator sees the live phase; create -f reads only spec and ignores it.
		"status:", "phase: running",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("yaml output missing %q:\n%s", want, stdout)
		}
	}
	// Server identity / owner fields must never leak into the apply-ready clone.
	if strings.Contains(stdout, "id:") || strings.Contains(stdout, "owner") {
		t.Errorf("yaml output leaked server fields:\n%s", stdout)
	}
}

// TestVMGetOutputYAMLCloudInit asserts the cloud-init payloads surface in the
// `vm get -o yaml` projection (an apply-ready clone manifest) when the server
// returns them, so a get | create -f round-trip reproduces the cloud-init.
func TestVMGetOutputYAMLCloudInit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"` + uuid.NewString() + `","name":"vm-ci","owner_id":"` + uuid.NewString() +
			`","image_url":"https://img.example/noble.qcow2","format":"qcow2","pool":"default","node":null,` +
			`"networks":[],"architecture":"amd64","vcpus":2,"memory_mib":2048,"status":{"phase":"running"},` +
			`"desired_phase":"running","user_data":"#cloud-config\nhostname: ci\n",` +
			`"network_config":"version: 2\n","created_at":"2026-06-01T10:00:00Z","updated_at":"2026-06-01T10:00:00Z"}`))
	}))
	defer srv.Close()

	stdout, _, err := runVMCmd(t, srv.URL, []string{"get", "vm-ci", "-o", "yaml"})
	if err != nil {
		t.Fatalf("get -o yaml error = %v", err)
	}
	for _, want := range []string{"userData:", "#cloud-config", "networkConfig:", "version: 2"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("yaml output missing %q:\n%s", want, stdout)
		}
	}
}
