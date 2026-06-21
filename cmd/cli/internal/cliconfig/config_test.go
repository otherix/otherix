// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cliconfig_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/otherix/otherix/cmd/cli/internal/cliconfig"
)

func TestLoad_MissingFileReturnsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.yaml")
	cfg, err := cliconfig.Load(path)
	if err != nil {
		t.Fatalf("Load(missing) error = %v, want nil", err)
	}
	if cfg.APIVersion != cliconfig.CurrentAPIVersion || cfg.Kind != cliconfig.Kind {
		t.Errorf("Load(missing) returned %+v; want apiVersion=%q kind=%q populated",
			cfg, cliconfig.CurrentAPIVersion, cliconfig.Kind)
	}
	if len(cfg.Clusters) != 0 || cfg.CurrentCluster != "" {
		t.Errorf("Load(missing) returned non-empty config: %+v", cfg)
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	in := &cliconfig.Config{
		CurrentCluster: "production",
		Clusters: []cliconfig.Cluster{
			{Name: "production", Server: "https://prod.example.com", Token: "otx_prod"},
			{Name: "staging", Server: "http://localhost:8080", Token: "otx_stg", TokenID: "deadbeef-..."},
		},
	}
	if err := cliconfig.Save(path, in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out, err := cliconfig.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Save populates apiVersion / kind; account for that in the want.
	want := *in
	want.APIVersion = cliconfig.CurrentAPIVersion
	want.Kind = cliconfig.Kind
	if diff := cmp.Diff(&want, out); diff != "" {
		t.Errorf("round-trip mismatch (-want +got):\n%s", diff)
	}
}

func TestSave_FilePermsAre0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := cliconfig.Save(path, &cliconfig.Config{}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("config file perms = %#o, want 0600", got)
	}
}

func TestSave_CreatesParentDirWith0700(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "nested", "deeper", "config.yaml")
	if err := cliconfig.Save(path, &cliconfig.Config{}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("Stat parent: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("parent dir perms = %#o, want 0700", got)
	}
}

func TestSave_AtomicReplace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	first := &cliconfig.Config{
		CurrentCluster: "a",
		Clusters:       []cliconfig.Cluster{{Name: "a", Server: "http://a", Token: "ta"}},
	}
	if err := cliconfig.Save(path, first); err != nil {
		t.Fatalf("Save first: %v", err)
	}
	second := &cliconfig.Config{
		CurrentCluster: "b",
		Clusters: []cliconfig.Cluster{
			{Name: "a", Server: "http://a", Token: "ta"},
			{Name: "b", Server: "http://b", Token: "tb"},
		},
	}
	if err := cliconfig.Save(path, second); err != nil {
		t.Fatalf("Save second: %v", err)
	}
	out, err := cliconfig.Load(path)
	if err != nil {
		t.Fatalf("Load after rewrite: %v", err)
	}
	if out.CurrentCluster != "b" || len(out.Clusters) != 2 {
		t.Errorf("rewrite not visible: %+v", out)
	}
	// Confirm no leftover temp files (sibling .otherix-config-* would
	// indicate cleanup failure).
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(path) {
			t.Errorf("stray temp file after Save: %s", e.Name())
		}
	}
}

func TestLoad_MalformedYAMLReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.yaml")
	if err := os.WriteFile(path, []byte("not: valid: yaml: at all:\n  - ["), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := cliconfig.Load(path); err == nil {
		t.Errorf("Load(malformed) returned nil, want error")
	}
}

func TestAddCluster_RejectsDuplicate(t *testing.T) {
	c := &cliconfig.Config{}
	if err := c.AddCluster(cliconfig.Cluster{Name: "x", Server: "u", Token: "t"}); err != nil {
		t.Fatalf("AddCluster first: %v", err)
	}
	err := c.AddCluster(cliconfig.Cluster{Name: "x", Server: "u2", Token: "t2"})
	if !errors.Is(err, cliconfig.ErrClusterExists) {
		t.Errorf("AddCluster duplicate err = %v, want ErrClusterExists", err)
	}
}

func TestRemoveCluster_AdvancesCurrentAlphabetically(t *testing.T) {
	c := &cliconfig.Config{
		CurrentCluster: "beta",
		Clusters: []cliconfig.Cluster{
			{Name: "alpha"},
			{Name: "beta"},
			{Name: "gamma"},
		},
	}
	if err := c.RemoveCluster("beta"); err != nil {
		t.Fatalf("RemoveCluster: %v", err)
	}
	if c.CurrentCluster != "alpha" {
		t.Errorf("CurrentCluster after removing beta = %q, want %q", c.CurrentCluster, "alpha")
	}
}

func TestRemoveCluster_ClearsCurrentWhenLast(t *testing.T) {
	c := &cliconfig.Config{
		CurrentCluster: "only",
		Clusters:       []cliconfig.Cluster{{Name: "only"}},
	}
	if err := c.RemoveCluster("only"); err != nil {
		t.Fatalf("RemoveCluster: %v", err)
	}
	if c.CurrentCluster != "" {
		t.Errorf("CurrentCluster after removing the last cluster = %q, want empty", c.CurrentCluster)
	}
}

func TestRemoveCluster_KeepsCurrentWhenRemovingOther(t *testing.T) {
	c := &cliconfig.Config{
		CurrentCluster: "a",
		Clusters:       []cliconfig.Cluster{{Name: "a"}, {Name: "b"}},
	}
	if err := c.RemoveCluster("b"); err != nil {
		t.Fatalf("RemoveCluster: %v", err)
	}
	if c.CurrentCluster != "a" {
		t.Errorf("CurrentCluster = %q after removing non-current; want unchanged", c.CurrentCluster)
	}
}

func TestSetCurrent_RejectsUnknownName(t *testing.T) {
	c := &cliconfig.Config{Clusters: []cliconfig.Cluster{{Name: "a"}}}
	err := c.SetCurrent("ghost")
	if !errors.Is(err, cliconfig.ErrClusterNotFound) {
		t.Errorf("SetCurrent unknown err = %v, want ErrClusterNotFound", err)
	}
}

func TestCurrentClusterEntry_Empty(t *testing.T) {
	c := &cliconfig.Config{}
	_, err := c.CurrentClusterEntry()
	if !errors.Is(err, cliconfig.ErrEmptyConfig) {
		t.Errorf("CurrentClusterEntry on empty err = %v, want ErrEmptyConfig", err)
	}
}

func TestCurrentClusterEntry_DanglingPointer(t *testing.T) {
	c := &cliconfig.Config{
		CurrentCluster: "ghost",
		Clusters:       []cliconfig.Cluster{{Name: "real"}},
	}
	_, err := c.CurrentClusterEntry()
	if !errors.Is(err, cliconfig.ErrClusterNotFound) {
		t.Errorf("CurrentClusterEntry dangling err = %v, want ErrClusterNotFound", err)
	}
}

func TestClusterRoundTripsInlineCA(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	cfg := &cliconfig.Config{}
	if err := cfg.AddCluster(cliconfig.Cluster{
		Name:                     "prod",
		Server:                   "https://cp.example:8080",
		Token:                    "otx_abc",
		CertificateAuthorityData: "YmFzZTY0LXBlbQ==",
		InsecureSkipTLSVerify:    true,
	}); err != nil {
		t.Fatalf("AddCluster: %v", err)
	}
	if err := cliconfig.Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := cliconfig.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	c, err := got.FindCluster("prod")
	if err != nil {
		t.Fatalf("FindCluster: %v", err)
	}
	if c.CertificateAuthorityData != "YmFzZTY0LXBlbQ==" {
		t.Errorf("CertificateAuthorityData = %q, want base64 round-trip", c.CertificateAuthorityData)
	}
	if !c.InsecureSkipTLSVerify {
		t.Errorf("InsecureSkipTLSVerify = false, want true")
	}
}
