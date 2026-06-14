// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package qemu

import (
	"encoding/json"
	"testing"
)

func TestNBDServerStartCmd_LegacyAddrShape(t *testing.T) {
	got := nbdServerStartCmd("0.0.0.0", 49153, "tls0", "authz0")
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	args := m["arguments"].(map[string]any)
	addr := args["addr"].(map[string]any)
	if addr["type"] != "inet" {
		t.Errorf("addr.type = %v, want inet", addr["type"])
	}
	data := addr["data"].(map[string]any)
	if data["port"] != "49153" { // STRING, legacy tagged form
		t.Errorf("addr.data.port = %v (%T), want string \"49153\"", data["port"], data["port"])
	}
	if args["tls-creds"] != "tls0" || args["tls-authz"] != "authz0" {
		t.Errorf("tls fields = %v/%v", args["tls-creds"], args["tls-authz"])
	}
}

func TestBlockdevAddNBDCmd_FlatAddr_StringPort(t *testing.T) {
	got := blockdevAddNBDCmd("mirror-target", "10.0.0.2", 49153, "migrate-disk0", "tls0", "node-b.agents.otherix.local")
	var m map[string]any
	_ = json.Unmarshal(got, &m)
	args := m["arguments"].(map[string]any)
	srv := args["server"].(map[string]any)
	if _, nested := srv["data"]; nested {
		t.Errorf("blockdev-add server must be FLAT SocketAddress, got nested data: %v", srv)
	}
	if srv["host"] != "10.0.0.2" || srv["port"] != "49153" {
		t.Errorf("server = %v, want flat host/port string", srv)
	}
	if args["tls-hostname"] != "node-b.agents.otherix.local" {
		t.Errorf("tls-hostname = %v", args["tls-hostname"])
	}
}

func TestBlockJobCancelCmd_ForceFlag(t *testing.T) {
	final := blockJobCancelCmd("mirror-disk0", false)
	abort := blockJobCancelCmd("mirror-disk0", true)
	var f, a map[string]any
	_ = json.Unmarshal(final, &f)
	_ = json.Unmarshal(abort, &a)
	if f["arguments"].(map[string]any)["force"] != false {
		t.Error("finalize must be force=false")
	}
	if a["arguments"].(map[string]any)["force"] != true {
		t.Error("abort must be force=true")
	}
}

func TestMigrateCancelCmd_Underscore(t *testing.T) {
	var m map[string]any
	_ = json.Unmarshal(migrateCancelCmd(), &m)
	if m["execute"] != "migrate_cancel" { // underscore, not hyphen
		t.Errorf("execute = %v, want migrate_cancel", m["execute"])
	}
}
