// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	"github.com/spf13/cobra"

	"github.com/otherix/otherix/internal/logger"
)

// defaultJoinTokenDest is where `join` writes the join-token plaintext (0600)
// and what cluster_join.token_path references. Keeping the secret in its own
// file (rather than inline in cluster_join.token) keeps it out of the
// world-readable api.yaml.
const defaultJoinTokenDest = "/var/lib/otherix/cluster-join-token" //nolint:gosec // G101 false positive: a destination path, not a credential

// restartUnit restarts the otherix-api systemd unit so the daemon re-reads the
// rewritten config and runs the join boot path. It is a package var so tests
// can swap in a spy instead of shelling out to systemd.
var restartUnit = func(log *slog.Logger) error {
	cmd := exec.Command("systemctl", "restart", "otherix-api.service")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl restart otherix-api: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// newJoinCommand builds the `otherix-api join` subcommand. One-shot and
// host-local: invoked from an operator shell on a fresh replica, it writes the
// cluster_join block + etcd.mode=join (and a unique etcd.name) into the
// existing api.yaml and restarts the unit. The daemon's serve boot path does
// the actual token redemption against an existing replica at the next start -
// this subcommand performs no network call.
func newJoinCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "join",
		Short: "Configure this control plane to join an existing HA cluster",
		Long: `One-shot, host-local join subcommand. Reads operator-supplied flags
and rewrites the existing api.yaml: it sets etcd.mode=join, a unique
etcd.name, and the cluster_join block (cp_url, token_path, ca_fingerprint),
then restarts the otherix-api unit. Every other key in the config file is
preserved.

This subcommand performs NO network call. The actual cluster CA fetch and
etcd member registration happen in the daemon's serve boot path at the next
start (ensureClusterCAForJoin -> FetchClusterCA -> buildInitialCluster).

Idempotent: re-running on a host already configured for join exits 0 without
restarting, unless --force is given.

Examples:
  # Token literal:
  otherix-api join \
    --cp-url=https://cp.example:8443 \
    --token=otx_join_... --ca-fingerprint=sha256:... \
    --name=otherix-1

  # Token from file:
  otherix-api join --token-path=/etc/otherix/join-token \
    --cp-url=https://cp.example:8443 --ca-fingerprint=sha256:...`,
		Args: cobra.NoArgs,
		RunE: runJoinCmd,
	}
	flags := cmd.Flags()
	flags.String("cp-url", "", "base URL of an existing replica to join (https://...)")
	flags.String("token", "", "cluster join token plaintext (mutually exclusive with --token-path)")
	flags.String("token-path", "", "path to a file holding the cluster join token (whitespace-trimmed)")
	flags.String("ca-fingerprint", "", "pinned cluster CA sha256 fingerprint (sha256:<hex> or bare hex)")
	flags.String("name", "", "unique etcd member name (default: hostname)")
	flags.String("config", defaultConfigPath, "path to the api.yaml to rewrite")
	flags.String("token-dest", defaultJoinTokenDest, "path the join token is written to (0600) and referenced from")
	flags.Bool("no-restart", false, "write config but do not restart the unit")
	flags.Bool("force", false, "re-join even if already configured for join")
	return cmd
}

// joinInputs is the validated CLI flag bundle for the join subcommand.
type joinInputs struct {
	cpURL         string
	token         string
	caFingerprint string
	name          string
	configPath    string
	tokenPath     string
	noRestart     bool
	force         bool
}

func runJoinCmd(cmd *cobra.Command, _ []string) error {
	in, err := readJoinInputs(cmd)
	if err != nil {
		return err
	}

	log := logger.WithComponent(
		logger.New(logger.Config{Level: "info", Format: "json"}),
		componentName,
	)

	// Idempotency: a host already configured for join no-ops (no restart)
	// unless --force is set. Read etcd.mode straight from the file with bare
	// koanf - NOT config.LoadAPI, which runs full Validate + env overlay and
	// would fall through to a needless rewrite + restart whenever the standalone
	// file does not validate on its own (e.g. jwt_secret supplied via env, a
	// common HA shape). Idempotency must fail toward inaction, not bounce a
	// healthy joined replica.
	if !in.force {
		if mode, name, merr := configEtcdIdentity(in.configPath); merr == nil && mode == "join" {
			log.Info("already configured for join; nothing to do (use --force to re-join)",
				slog.String("config_path", in.configPath),
				slog.String("etcd_name", name))
			return nil
		}
	}

	// Write the token plaintext to its own 0600 file and reference it via
	// cluster_join.token_path, keeping the secret out of the api.yaml.
	if err := os.MkdirAll(filepath.Dir(in.tokenPath), 0o750); err != nil {
		return fmt.Errorf("create token dir: %v", err)
	}
	if err := os.WriteFile(in.tokenPath, []byte(in.token+"\n"), 0o600); err != nil {
		return fmt.Errorf("write join token: %v", err)
	}

	if err := writeJoinConfig(in); err != nil {
		return fmt.Errorf("rewrite %s: %v", in.configPath, err)
	}

	log.Info("wrote cluster_join config",
		slog.String("config_path", in.configPath),
		slog.String("etcd_name", in.name),
		slog.String("cp_url", in.cpURL),
		slog.String("token_path", in.tokenPath))

	if in.noRestart {
		log.Info("--no-restart set; restart the unit manually to join",
			slog.String("command", "systemctl restart otherix-api.service"))
		return nil
	}

	if err := restartUnit(log); err != nil {
		log.Warn("restart failed; config is written - restart the unit manually to join",
			slog.String("command", "systemctl restart otherix-api.service"),
			slog.String("error", err.Error()))
		return err
	}
	log.Info("restarted otherix-api unit; join proceeds on the next boot")
	return nil
}

// readJoinInputs gathers flags, resolves the token via the --token /
// --token-path mux, defaults the member name to the hostname, and rejects
// invalid combinations. Returns a fully-formed joinInputs ready for use.
func readJoinInputs(cmd *cobra.Command) (joinInputs, error) {
	flags := cmd.Flags()
	in := joinInputs{}
	in.cpURL, _ = flags.GetString("cp-url")
	in.caFingerprint, _ = flags.GetString("ca-fingerprint")
	in.name, _ = flags.GetString("name")
	in.configPath, _ = flags.GetString("config")
	in.tokenPath, _ = flags.GetString("token-dest")
	in.noRestart, _ = flags.GetBool("no-restart")
	in.force, _ = flags.GetBool("force")

	if in.cpURL == "" {
		return joinInputs{}, errors.New("--cp-url is required")
	}
	if in.caFingerprint == "" {
		return joinInputs{}, errors.New("--ca-fingerprint is required")
	}

	tokenLit, _ := flags.GetString("token")
	tokenPath, _ := flags.GetString("token-path")
	token, err := resolveJoinToken(tokenLit, tokenPath)
	if err != nil {
		return joinInputs{}, err
	}
	in.token = token

	if in.name == "" {
		host, err := os.Hostname()
		if err != nil || host == "" {
			return joinInputs{}, errors.New("--name is required (could not derive a default from the hostname)")
		}
		in.name = host
	}
	return in, nil
}

// resolveJoinToken returns the join-token plaintext from the --token literal or
// the --token-path file. Exactly one source must be set; the file read is
// whitespace-trimmed.
func resolveJoinToken(tokenLit, tokenPath string) (string, error) {
	switch {
	case tokenLit != "" && tokenPath != "":
		return "", errors.New("--token and --token-path are mutually exclusive - specify exactly one")
	case tokenLit != "":
		return tokenLit, nil
	case tokenPath != "":
		raw, err := os.ReadFile(tokenPath) //nolint:gosec // operator-supplied path
		if err != nil {
			return "", fmt.Errorf("read --token-path %s: %v", tokenPath, err)
		}
		s := strings.TrimSpace(string(raw))
		if s == "" {
			return "", fmt.Errorf("--token-path %s is empty after trimming whitespace", tokenPath)
		}
		return s, nil
	default:
		return "", errors.New("one of --token / --token-path is required")
	}
}

// configEtcdIdentity reads etcd.mode and etcd.name straight from the api.yaml
// file with bare koanf - no defaults overlay, no env overlay, no Validate. The
// idempotency guard keys on this so a standalone file that does not validate on
// its own (env-supplied secrets are the common HA case) does not get
// misclassified as "not yet joined" and needlessly rewritten + restarted.
func configEtcdIdentity(path string) (mode, name string, err error) {
	k := koanf.New(".")
	if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
		return "", "", err
	}
	return k.String("etcd.mode"), k.String("etcd.name"), nil
}

// writeJoinConfig surgically rewrites the existing api.yaml: it koanf-loads
// only the file's keys (not LoadAPI's defaults), sets etcd.mode=join,
// etcd.name, and the cluster_join block, then marshals back to YAML and writes
// it atomically (temp + rename in the destination dir, 0644). Every other key
// in the file survives the round-trip.
func writeJoinConfig(in joinInputs) error {
	k := koanf.New(".")
	if err := k.Load(file.Provider(in.configPath), yaml.Parser()); err != nil {
		return fmt.Errorf("read config %q: %v", in.configPath, err)
	}

	_ = k.Set("etcd.mode", "join")
	_ = k.Set("etcd.name", in.name)
	_ = k.Set("cluster_join.cp_url", in.cpURL)
	_ = k.Set("cluster_join.token_path", in.tokenPath)
	_ = k.Set("cluster_join.ca_fingerprint", in.caFingerprint)

	b, err := k.Marshal(yaml.Parser())
	if err != nil {
		return fmt.Errorf("marshal config: %v", err)
	}
	return atomicWriteFile(in.configPath, b, 0o644)
}

// atomicWriteFile writes data to path via a temp file in the same directory
// followed by a rename, so a concurrent reader never observes a partial write.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".api-join-*.yaml")
	if err != nil {
		return fmt.Errorf("create temp file: %v", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %v", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %v", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp file: %v", err)
	}
	return nil
}
