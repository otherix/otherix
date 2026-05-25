// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package cliconfig owns the on-disk operator credential store for
// the otherix CLI. The file format is a small YAML document
// (apiVersion / kind / clusters / current-cluster) inspired by kubectl
// and docker config — see doc.go for the rationale and the trade-off
// against a richer file format.
//
// This package owns three concerns: the typed file shape (Config /
// Cluster), the atomic on-disk lifecycle (Load / Save), and the
// runtime precedence chain that turns a request ("which cluster?
// which token? which endpoint?") into a resolved (endpoint, token)
// pair (Resolve, in lookup.go). The cobra `otherix config …`
// subcommands sit on top of this; they own the user-facing UX, never
// the file format.
package cliconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// CurrentAPIVersion is the schema version written into freshly-saved
// configs. The CLI accepts any value on load (forward-compat) but
// always writes the current revision.
const CurrentAPIVersion = "v1"

// Kind is the YAML `kind` field. Hard-coded to match kubectl-style
// "this is a Config document" sentinel — present so a future tool
// can dispatch by kind on a single file.
const Kind = "Config"

// filePerm and dirPerm are the on-disk modes Save enforces. The
// config holds long-lived API tokens; world-readable would expose
// them to other users on a shared host.
const (
	filePerm os.FileMode = 0o600
	dirPerm  os.FileMode = 0o700
)

// Config is the in-memory shape of the on-disk YAML document. The
// zero value is a valid empty config (no clusters, no current). Load
// always returns a usable Config — never nil — so callers can chain
// AddCluster / Save without nil-checks.
type Config struct {
	APIVersion     string    `yaml:"apiVersion"`
	Kind           string    `yaml:"kind"`
	CurrentCluster string    `yaml:"current-cluster"`
	Clusters       []Cluster `yaml:"clusters"`
}

// Cluster is one named CP endpoint plus the credential the CLI uses
// against it. TokenID is the uuid of the api_tokens row that issued
// Token — kept so `config add cluster --force` can best-effort
// revoke the displaced token via DELETE
// /v1/users/me/api-tokens/{token_id}. Old config files written
// before TokenID existed leave it empty; the revoke step skips with
// a warning in that case.
type Cluster struct {
	Name    string `yaml:"name"`
	Server  string `yaml:"server"`
	Token   string `yaml:"token"`
	TokenID string `yaml:"token-id,omitempty"`
}

// ErrClusterNotFound is returned by FindCluster / SetCurrent /
// RemoveCluster / CurrentClusterEntry when the named cluster is
// absent. Callers map this to a user-facing 'cluster "X" not in
// config' message.
var ErrClusterNotFound = errors.New("cluster not found")

// ErrClusterExists is returned by AddCluster when a cluster with the
// same Name is already present. `config add cluster` translates this
// to its actionable "use --force" suggestion.
var ErrClusterExists = errors.New("cluster already exists")

// ErrEmptyConfig is returned by CurrentClusterEntry when the config
// has no clusters at all. Distinguishes "you have not run config add
// cluster yet" from "your current-cluster pointer is wrong".
var ErrEmptyConfig = errors.New("config has no clusters")

// Load reads path and returns a populated Config. A missing file
// yields an empty Config (APIVersion / Kind pre-filled to the
// current revision) and a nil error — bootstrapping is part of the
// normal flow. Malformed YAML or an unreadable file (other than
// ENOENT) returns the underlying error wrapped.
func Load(path string) (*Config, error) {
	// Path is operator-controlled (via --config flag, $OTHERIX_CONFIG,
	// or $HOME-derived default) — there is no untrusted source. The
	// gosec G304 check flags os.ReadFile of any variable path, but
	// reading the operator's own credential store is the package's job.
	raw, err := os.ReadFile(path) //nolint:gosec
	if errors.Is(err, os.ErrNotExist) {
		return &Config{APIVersion: CurrentAPIVersion, Kind: Kind}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cliconfig: read %s: %w", path, err)
	}
	cfg := Config{}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("cliconfig: parse %s: %w", path, err)
	}
	if cfg.APIVersion == "" {
		cfg.APIVersion = CurrentAPIVersion
	}
	if cfg.Kind == "" {
		cfg.Kind = Kind
	}
	return &cfg, nil
}

// Save writes cfg to path atomically: marshal → write to a sibling
// temp file with 0600 perms → fsync → rename onto path. The parent
// directory is created with 0700 if missing. Atomicity matters
// because `config add cluster` and `config remove` rewrite the file
// in place; a crash mid-write must never leave a half-written
// credential store.
func Save(path string, cfg *Config) error {
	if cfg.APIVersion == "" {
		cfg.APIVersion = CurrentAPIVersion
	}
	if cfg.Kind == "" {
		cfg.Kind = Kind
	}
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return fmt.Errorf("cliconfig: mkdir %s: %w", filepath.Dir(path), err)
	}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("cliconfig: marshal: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".otherix-config-*")
	if err != nil {
		return fmt.Errorf("cliconfig: create temp: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup on any failure path. The successful path
	// renames the file, after which Remove is a no-op (ENOENT
	// swallowed below).
	defer func() {
		_ = os.Remove(tmpName)
	}()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cliconfig: write temp: %w", err)
	}
	if err := tmp.Chmod(filePerm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cliconfig: chmod temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cliconfig: fsync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cliconfig: close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("cliconfig: rename onto %s: %w", path, err)
	}
	return nil
}

// FindCluster returns a pointer to the named entry, or
// ErrClusterNotFound. The pointer aliases into c.Clusters — mutating
// it mutates the config; Save persists the change.
func (c *Config) FindCluster(name string) (*Cluster, error) {
	for i := range c.Clusters {
		if c.Clusters[i].Name == name {
			return &c.Clusters[i], nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrClusterNotFound, name)
}

// AddCluster appends entry, refusing on a name collision. Callers
// that intend to overwrite must RemoveCluster first.
func (c *Config) AddCluster(entry Cluster) error {
	if _, err := c.FindCluster(entry.Name); err == nil {
		return fmt.Errorf("%w: %q", ErrClusterExists, entry.Name)
	}
	c.Clusters = append(c.Clusters, entry)
	return nil
}

// RemoveCluster deletes the entry by name. When the removed entry
// was the CurrentCluster, the pointer is advanced to the first
// remaining cluster (alphabetical by Name) or cleared if none
// remain — operators see the change reflected in `config list`
// immediately.
func (c *Config) RemoveCluster(name string) error {
	idx := -1
	for i := range c.Clusters {
		if c.Clusters[i].Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("%w: %q", ErrClusterNotFound, name)
	}
	c.Clusters = append(c.Clusters[:idx], c.Clusters[idx+1:]...)
	if c.CurrentCluster != name {
		return nil
	}
	if len(c.Clusters) == 0 {
		c.CurrentCluster = ""
		return nil
	}
	first := c.Clusters[0].Name
	for _, e := range c.Clusters[1:] {
		if e.Name < first {
			first = e.Name
		}
	}
	c.CurrentCluster = first
	return nil
}

// SetCurrent flips CurrentCluster to name, refusing when name is
// not in the cluster list. Callers that want "set current if
// missing" should AddCluster first.
func (c *Config) SetCurrent(name string) error {
	if _, err := c.FindCluster(name); err != nil {
		return err
	}
	c.CurrentCluster = name
	return nil
}

// CurrentClusterEntry returns the Cluster pointed to by
// CurrentCluster, or sentinels for the two empty cases:
// ErrEmptyConfig (no clusters at all) and ErrClusterNotFound (the
// current-cluster pointer references a name that no longer exists —
// usually after a hand-edit). Callers that just want "any usable
// cluster" should handle both sentinels uniformly.
func (c *Config) CurrentClusterEntry() (*Cluster, error) {
	if len(c.Clusters) == 0 {
		return nil, ErrEmptyConfig
	}
	if c.CurrentCluster == "" {
		return nil, fmt.Errorf("%w: current-cluster is unset", ErrClusterNotFound)
	}
	return c.FindCluster(c.CurrentCluster)
}
