// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	"github.com/otherix/otherix/cmd/cli/vm"
)

// runVMCmd mounts the `vm` subcommand tree on a throwaway parent with
// the persistent
// flags the real root provides, then executes args. Returns captured
// stdout / stderr and the cobra error.
func runVMCmd(t *testing.T, endpoint string, args []string) (stdout, stderr string, err error) {
	t.Helper()
	parent := vm.NewCommand()
	parent.PersistentFlags().String(cliauth.FlagConfig, "", "")
	parent.PersistentFlags().String(cliauth.FlagEndpoint, "", "")
	parent.PersistentFlags().String(cliauth.FlagToken, "", "")
	parent.PersistentFlags().String(cliauth.FlagCluster, "", "")

	full := append([]string{"--endpoint", endpoint, "--token", "test-token"}, args...)
	parent.SetArgs(full)
	parent.SilenceUsage = true
	parent.SilenceErrors = true
	var out, errBuf bytes.Buffer
	parent.SetOut(&out)
	parent.SetErr(&errBuf)
	parent.SetContext(context.Background())
	err = parent.Execute()
	return out.String(), errBuf.String(), err
}

// createdVMJSON is the 201 admission-only body POST /v1/vms now returns:
// the VM view in the pending phase (no task envelope). name echoes the
// request so the create summary line is deterministic.
func createdVMJSON(name string) []byte {
	return []byte(`{"id":"` + uuid.NewString() + `","name":"` + name + `","owner_id":"` + uuid.NewString() +
		`","pool":"default","architecture":"amd64","vcpus":2,"memory_mb":2048,` +
		`"status":{"phase":"pending","reason":"pending_schedule"},"desired_phase":"running",` +
		`"labels":{},"created_at":"2026-05-10T10:00:00Z","updated_at":"2026-05-10T10:00:00Z"}`)
}

// TestVMCreate_UserDataFile sends --user-data=<path> and asserts the
// CP request body carries the resolved YAML in user_data with
// cloud_init_disabled false. Locks in operator UX iteration's primary
// path (file source) under the renamed flag.
func TestVMCreate_UserDataFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "ci.yaml")
	ciBody := "#cloud-config\nusers:\n  - name: from-file\n"
	if err := os.WriteFile(yamlPath, []byte(ciBody), 0o600); err != nil {
		t.Fatalf("seed yaml: %v", err)
	}

	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/vms" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(createdVMJSON("vm-ci-file"))
	}))
	defer srv.Close()

	_, _, err := runVMCmd(t, srv.URL, []string{
		"create",
		"vm-ci-file",
		"--image-url", "https://example.com/ubuntu.qcow2",
		"--arch", "amd64",
		"--pool", "pool-dev",
		"--vcpus", "2",
		"--memory-mb", "512",
		"--user-data", yamlPath,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := captured["user_data"]; got != ciBody {
		t.Errorf("user_data = %v, want %q", got, ciBody)
	}
	if disabled, _ := captured["cloud_init_disabled"].(bool); disabled {
		t.Errorf("cloud_init_disabled = true, want false when --no-cloud-init not set")
	}
	if got := captured["image_url"]; got != "https://example.com/ubuntu.qcow2" {
		t.Errorf("image_url = %v, want the --image-url value", got)
	}
	if got := captured["arch"]; got != "amd64" {
		t.Errorf("arch = %v, want amd64", got)
	}
}

// TestVMCreate_OldCloudInitFlagRemoved locks in the clean break: the old
// --cloud-init flag is gone (no hidden alias), so passing it is an
// "unknown flag" error and no HTTP call leaves the box.
func TestVMCreate_OldCloudInitFlagRemoved(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("HTTP call must not happen when --cloud-init is rejected")
	}))
	defer srv.Close()

	_, _, err := runVMCmd(t, srv.URL, []string{
		"create",
		"vm-old-flag",
		"--image-url", "https://example.com/ubuntu.qcow2",
		"--arch", "amd64",
		"--cloud-init", "/tmp/whatever.yaml",
	})
	if err == nil {
		t.Fatalf("expected unknown-flag error for --cloud-init")
	}
	if !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("err = %v, want mention of 'unknown flag'", err)
	}
}

// TestVMCreate_NetworkConfigFile sends --network-config=<path> and
// asserts the CP request body carries the resolved netplan YAML in
// network_config. The content is opaque to the CLI (netplan v2, not
// #cloud-config), so no validator warnings gate it.
func TestVMCreate_NetworkConfigFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ncPath := filepath.Join(dir, "nc.yaml")
	ncBody := "version: 2\nethernets:\n  eth0:\n    dhcp4: true\n"
	if err := os.WriteFile(ncPath, []byte(ncBody), 0o600); err != nil {
		t.Fatalf("seed yaml: %v", err)
	}

	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/vms" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(createdVMJSON("vm-nc-file"))
	}))
	defer srv.Close()

	_, _, err := runVMCmd(t, srv.URL, []string{
		"create",
		"vm-nc-file",
		"--image-url", "https://example.com/ubuntu.qcow2",
		"--arch", "amd64",
		"--pool", "pool-dev",
		"--vcpus", "2",
		"--memory-mb", "512",
		"--network-config", ncPath,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := captured["network_config"]; got != ncBody {
		t.Errorf("network_config = %v, want %q", got, ncBody)
	}
	if _, present := captured["user_data"]; present {
		t.Errorf("user_data unexpectedly present when only --network-config set: %v", captured["user_data"])
	}
}

// TestVMCreate_NetworkConfigMutualExclusion locks in the CLI-level guard:
// supplying both --network-config AND --no-cloud-init fails before any
// HTTP call leaves the box.
func TestVMCreate_NetworkConfigMutualExclusion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ncPath := filepath.Join(dir, "nc.yaml")
	if err := os.WriteFile(ncPath, []byte("version: 2\n"), 0o600); err != nil {
		t.Fatalf("seed yaml: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("HTTP call must not happen when mutual-exclusion guard fires")
	}))
	defer srv.Close()

	_, _, err := runVMCmd(t, srv.URL, []string{
		"create",
		"vm-nc-conflict",
		"--image-url", "https://example.com/ubuntu.qcow2",
		"--arch", "amd64",
		"--pool", "pool-dev",
		"--vcpus", "1",
		"--memory-mb", "128",
		"--network-config", ncPath,
		"--no-cloud-init",
	})
	if err == nil {
		t.Fatalf("expected mutual-exclusion error")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("err = %v, want mention of 'mutually exclusive'", err)
	}
}

// TestVMCreate_UserDataAndNetworkConfig sends both --user-data and
// --network-config and asserts the request body carries BOTH fields with
// no error: the two cloud-init channels are independent and composable.
func TestVMCreate_UserDataAndNetworkConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	udPath := filepath.Join(dir, "ud.yaml")
	udBody := "#cloud-config\nusers:\n  - name: from-file\n"
	if err := os.WriteFile(udPath, []byte(udBody), 0o600); err != nil {
		t.Fatalf("seed user-data: %v", err)
	}
	ncPath := filepath.Join(dir, "nc.yaml")
	ncBody := "version: 2\nethernets:\n  eth0:\n    dhcp4: true\n"
	if err := os.WriteFile(ncPath, []byte(ncBody), 0o600); err != nil {
		t.Fatalf("seed network-config: %v", err)
	}

	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/vms" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(createdVMJSON("vm-ud-nc"))
	}))
	defer srv.Close()

	_, _, err := runVMCmd(t, srv.URL, []string{
		"create",
		"vm-ud-nc",
		"--image-url", "https://example.com/ubuntu.qcow2",
		"--arch", "amd64",
		"--pool", "pool-dev",
		"--vcpus", "2",
		"--memory-mb", "512",
		"--user-data", udPath,
		"--network-config", ncPath,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := captured["user_data"]; got != udBody {
		t.Errorf("user_data = %v, want %q", got, udBody)
	}
	if got := captured["network_config"]; got != ncBody {
		t.Errorf("network_config = %v, want %q", got, ncBody)
	}
}

// TestVMCreate_ImageFields drives create with the explicit image-source
// flags and asserts the CP request body carries every image field
// (image_url / image_sha256 / arch / firmware / format / disk_gib).
// Locks in the template-removal reshape: a VM is created directly from
// an image source, no template reference on the wire.
func TestVMCreate_ImageFields(t *testing.T) {
	t.Parallel()
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/vms" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(createdVMJSON("vm-img"))
	}))
	defer srv.Close()

	_, _, err := runVMCmd(t, srv.URL, []string{
		"create",
		"vm-img",
		"--image-url", "https://example.com/ubuntu.qcow2",
		"--image-sha256", "deadbeef",
		"--arch", "arm64",
		"--firmware", "uefi",
		"--format", "qcow2",
		"--disk-gib", "20",
		"--vcpus", "2",
		"--memory-mb", "2048",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	wants := map[string]any{
		"name":         "vm-img",
		"image_url":    "https://example.com/ubuntu.qcow2",
		"image_sha256": "deadbeef",
		"arch":         "arm64",
		"firmware":     "uefi",
		"format":       "qcow2",
		"disk_gib":     float64(20),
	}
	for k, want := range wants {
		if got := captured[k]; got != want {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}
	if _, present := captured["template"]; present {
		t.Errorf("template unexpectedly present in body: %v", captured["template"])
	}
}

// TestVMCreate_NameFlagRemoved locks in the positional-name reshape:
// the old --name flag is gone (clean break, no alias), so passing it
// is an "unknown flag" error and no HTTP call leaves the box.
func TestVMCreate_NameFlagRemoved(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("HTTP call must not happen when --name is rejected")
	}))
	defer srv.Close()

	_, _, err := runVMCmd(t, srv.URL, []string{
		"create",
		"--name", "vm-flag",
		"--image-url", "https://example.com/ubuntu.qcow2",
		"--arch", "amd64",
	})
	if err == nil {
		t.Fatalf("expected unknown-flag error for --name")
	}
	if !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("err = %v, want mention of 'unknown flag'", err)
	}
}

// TestVMCreate_MissingName asserts create fails before any HTTP call
// when the positional name argument is omitted.
func TestVMCreate_MissingName(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("HTTP call must not happen when the name argument is missing")
	}))
	defer srv.Close()

	_, _, err := runVMCmd(t, srv.URL, []string{
		"create",
		"--image-url", "https://example.com/ubuntu.qcow2",
		"--arch", "amd64",
	})
	if err == nil {
		t.Fatalf("expected error for missing name argument")
	}
}

// TestVMCreate_MissingImageURL asserts create fails before any HTTP
// call when --image-url is omitted.
func TestVMCreate_MissingImageURL(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("HTTP call must not happen when --image-url is missing")
	}))
	defer srv.Close()

	_, _, err := runVMCmd(t, srv.URL, []string{
		"create",
		"vm-no-image",
		"--arch", "amd64",
		"--vcpus", "1",
		"--memory-mb", "128",
	})
	if err == nil {
		t.Fatalf("expected error for missing --image-url")
	}
	if !strings.Contains(err.Error(), "image-url") {
		t.Errorf("err = %v, want mention of 'image-url'", err)
	}
}

// TestVMCreate_MissingArch asserts create fails before any HTTP call
// when --arch is omitted.
func TestVMCreate_MissingArch(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("HTTP call must not happen when --arch is missing")
	}))
	defer srv.Close()

	_, _, err := runVMCmd(t, srv.URL, []string{
		"create",
		"vm-no-arch",
		"--image-url", "https://example.com/ubuntu.qcow2",
		"--vcpus", "1",
		"--memory-mb", "128",
	})
	if err == nil {
		t.Fatalf("expected error for missing --arch")
	}
	if !strings.Contains(err.Error(), "arch") {
		t.Errorf("err = %v, want mention of 'arch'", err)
	}
}

// TestVMCreate_CloudInitStdin sends --user-data=- and pipes content
// through os.Stdin. The cobra command does not let us inject stdin
// directly, but the cloudinit package's stdinReader var is swapped
// in the helper test; here we only verify the CLI passes "-" through
// as a path, and that ReadSource's actual stdin path is covered by
// TestReadSource in the helper package.
func TestVMCreate_CloudInitStdin_FlagAccepted(t *testing.T) {
	t.Parallel()
	// Empty stdin will be parsed as empty body (warning, no error); the
	// CLI sends user_data="" to the server, and the request still goes
	// through. The test asserts the dispatch happened — not the body
	// shape, since the stdin redirection lives in package-level state.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(createdVMJSON("vm-ci-stdin"))
	}))
	defer srv.Close()

	// Swap stdin to a deterministic, non-blocking buffer so the cobra
	// command does not hang waiting for input on the test runner's
	// terminal. ReadAll on an empty buffer returns nil, []byte{} —
	// Validate emits "empty body" warning, no error, and dispatch
	// proceeds.
	prevStdin := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	_ = w.Close()
	defer func() { os.Stdin = prevStdin }()

	_, _, err := runVMCmd(t, srv.URL, []string{
		"create",
		"vm-ci-stdin",
		"--image-url", "https://example.com/ubuntu.qcow2",
		"--arch", "amd64",
		"--pool", "pool-dev",
		"--vcpus", "1",
		"--memory-mb", "128",
		"--user-data", "-",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

// TestVMCreate_NoCloudInit sends --no-cloud-init and verifies the
// request body carries cloud_init_disabled=true with user_data absent.
func TestVMCreate_NoCloudInit(t *testing.T) {
	t.Parallel()
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(createdVMJSON("vm-ci-disabled"))
	}))
	defer srv.Close()

	_, _, err := runVMCmd(t, srv.URL, []string{
		"create",
		"vm-ci-disabled",
		"--image-url", "https://example.com/ubuntu.qcow2",
		"--arch", "amd64",
		"--pool", "pool-dev",
		"--vcpus", "2",
		"--memory-mb", "512",
		"--no-cloud-init",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if disabled, _ := captured["cloud_init_disabled"].(bool); !disabled {
		t.Errorf("cloud_init_disabled = %v, want true", captured["cloud_init_disabled"])
	}
	if _, present := captured["user_data"]; present {
		t.Errorf("user_data unexpectedly present in disable-only body: %v", captured["user_data"])
	}
}

// TestVMCreate_CloudInitMutualExclusion locks in the CLI-level guard:
// supplying both --user-data AND --no-cloud-init fails before any
// HTTP call leaves the box. DB CHECK + handler validation are the
// server-side backstops; this test covers the operator-friendly UX
// (failure without round-trip).
func TestVMCreate_CloudInitMutualExclusion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "ci.yaml")
	if err := os.WriteFile(yamlPath, []byte("#cloud-config\n"), 0o600); err != nil {
		t.Fatalf("seed yaml: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("HTTP call must not happen when mutual-exclusion guard fires")
	}))
	defer srv.Close()

	_, _, err := runVMCmd(t, srv.URL, []string{
		"create",
		"vm-conflict",
		"--image-url", "https://example.com/ubuntu.qcow2",
		"--arch", "amd64",
		"--pool", "pool-dev",
		"--vcpus", "1",
		"--memory-mb", "128",
		"--user-data", yamlPath,
		"--no-cloud-init",
	})
	if err == nil {
		t.Fatalf("expected mutual-exclusion error")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("err = %v, want mention of 'mutually exclusive'", err)
	}
}

// TestVMCreate_CloudInitMalformedYAML locks in the validator gating:
// a malformed YAML file is rejected by the CLI before dispatch.
func TestVMCreate_CloudInitMalformedYAML(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "ci.yaml")
	// Unterminated mapping → yaml.v3 parser error.
	if err := os.WriteFile(yamlPath, []byte("#cloud-config\nusers: [unterminated\n"), 0o600); err != nil {
		t.Fatalf("seed yaml: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("HTTP call must not happen when YAML validation fails")
	}))
	defer srv.Close()

	_, _, err := runVMCmd(t, srv.URL, []string{
		"create",
		"vm-bad-yaml",
		"--image-url", "https://example.com/ubuntu.qcow2",
		"--arch", "amd64",
		"--pool", "pool-dev",
		"--vcpus", "1",
		"--memory-mb", "128",
		"--user-data", yamlPath,
	})
	if err == nil {
		t.Fatalf("expected YAML parse error")
	}
	if !strings.Contains(err.Error(), "yaml_parse") {
		t.Errorf("err = %v, want mention of 'yaml_parse'", err)
	}
}
