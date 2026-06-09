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
	"syscall"

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

	// Idempotency: a host whose join actually COMPLETED no-ops (no restart)
	// unless --force is set. Read the identity straight from the file with bare
	// koanf - NOT config.LoadAPI, which runs full Validate + env overlay and
	// would fall through to a needless rewrite + restart whenever the standalone
	// file does not validate on its own (e.g. jwt_secret supplied via env, a
	// common HA shape). Idempotency must fail toward inaction, not bounce a
	// healthy joined replica.
	//
	// The no-op requires evidence the join completed, not merely that mode=join
	// was written: a healthy joined node has the cluster CA cert+key persisted
	// on disk (the daemon writes them after a successful redemption). When
	// mode=join but the CA is absent, the previous join FAILED (wrong token / CP
	// unreachable); fall through so a re-run with a corrected token re-applies
	// the token + config and restarts, instead of silently exiting 0 on the
	// stale flag (Rel-I1).
	if !in.force {
		if id, merr := configEtcdIdentity(in.configPath); merr == nil && id.mode == "join" {
			if fileExists(id.caCertFile) && fileExists(id.caKeyFile) {
				log.Info("already configured for join and cluster CA is on disk; nothing to do (use --force to re-join)",
					slog.String("config_path", in.configPath),
					slog.String("etcd_name", id.name))
				return nil
			}
			log.Info("configured for join but cluster CA is absent (previous join did not complete); re-applying token and restarting",
				slog.String("config_path", in.configPath),
				slog.String("etcd_name", id.name),
				slog.String("ca_cert_file", id.caCertFile))
		}
	}

	// Write the token plaintext to its own 0600 file and reference it via
	// cluster_join.token_path, keeping the secret out of the api.yaml.
	tokenDir := filepath.Dir(in.tokenPath)
	created, err := mkdirAllReport(tokenDir, 0o750)
	if err != nil {
		return fmt.Errorf("create token dir: %v", err)
	}
	if err := os.WriteFile(in.tokenPath, []byte(in.token+"\n"), 0o600); err != nil {
		return fmt.Errorf("write join token: %v", err)
	}

	// Sec-C1: `sudo otherix-api join` runs as root and would leave the token
	// file root:root 0600 - unreadable by the daemon, which runs as the
	// otherix user and reads cluster_join.token_path at boot. Chown the token
	// (and any dir we just created for a custom --token-dest) to match the
	// owner of the state dir (otherix in the packaged flow, the invoking user
	// in a same-uid dev run - a no-op). Mode stays 0600: the token redeems the
	// cluster CA private key, the highest-value secret, so it must not become
	// world-readable. Best-effort: a chown failure logs a WARN with the manual
	// fix hint and continues rather than hard-failing the join.
	chownToParentOwner(in.tokenPath, tokenDir, log)
	if created != "" {
		chownToParentOwner(created, filepath.Dir(created), log)
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

// joinIdentity is the bare-koanf view of the api.yaml the idempotency guard
// keys on: etcd.mode + etcd.name, plus the cluster_ca cert/key paths (the
// guard treats those files existing as proof the join completed).
type joinIdentity struct {
	mode       string
	name       string
	caCertFile string
	caKeyFile  string
}

// Default cluster CA file paths, mirroring config.defaultAPIConfig's ClusterCA.
// The packaged api.yaml omits cluster_ca and relies on these binary defaults,
// so the guard must default to the same paths when the file is silent.
const (
	defaultClusterCACertFile = "/var/lib/otherix/ca/cluster-ca.crt"
	defaultClusterCAKeyFile  = "/var/lib/otherix/ca/cluster-ca.key"
)

// configEtcdIdentity reads etcd.mode, etcd.name, and the cluster_ca cert/key
// paths straight from the api.yaml file with bare koanf - no defaults overlay,
// no env overlay, no Validate. The idempotency guard keys on this so a
// standalone file that does not validate on its own (env-supplied secrets are
// the common HA case) does not get misclassified as "not yet joined" and
// needlessly rewritten + restarted. The CA paths default to the standard
// /var/lib/otherix/ca/cluster-ca.{crt,key} when the file omits them (the
// packaged config relies on the binary default).
func configEtcdIdentity(path string) (joinIdentity, error) {
	k := koanf.New(".")
	if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
		return joinIdentity{}, err
	}
	id := joinIdentity{
		mode:       k.String("etcd.mode"),
		name:       k.String("etcd.name"),
		caCertFile: k.String("cluster_ca.cert_file"),
		caKeyFile:  k.String("cluster_ca.key_file"),
	}
	if id.caCertFile == "" {
		id.caCertFile = defaultClusterCACertFile
	}
	if id.caKeyFile == "" {
		id.caKeyFile = defaultClusterCAKeyFile
	}
	return id, nil
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

// mkdirAllReport behaves like os.MkdirAll but reports the shallowest directory
// it had to create (empty string when the full path already existed), so the
// caller can chown a freshly-created custom --token-dest dir to the daemon user.
func mkdirAllReport(path string, perm os.FileMode) (created string, err error) {
	if _, statErr := os.Stat(path); statErr == nil {
		return "", nil // already exists, nothing created
	}
	// Walk up to the first existing ancestor; that ancestor's first missing
	// child is the shallowest directory MkdirAll will create.
	shallowest := path
	for {
		parent := filepath.Dir(shallowest)
		if parent == shallowest {
			break // reached the root
		}
		if _, statErr := os.Stat(parent); statErr == nil {
			break // parent exists; shallowest is its first missing child
		}
		shallowest = parent
	}
	if err := os.MkdirAll(path, perm); err != nil {
		return "", err
	}
	return shallowest, nil
}

// chownToParentOwner chowns path to the uid/gid that owns parentDir, so a file
// (or dir) written by root inherits the same owner as the state directory (the
// otherix daemon user in the packaged flow). Best-effort: any stat/chown error,
// or a target already owned by the current process uid, logs at most a WARN with
// the manual fix hint and returns without failing the join.
func chownToParentOwner(path, parentDir string, log *slog.Logger) {
	st, err := os.Stat(parentDir)
	if err != nil {
		log.Warn("could not stat the state dir to inherit ownership for the join token; the daemon user may be unable to read it",
			slog.String("path", path),
			slog.String("parent_dir", parentDir),
			slog.String("error", err.Error()),
			slog.String("manual_fix", "chown otherix:otherix "+path))
		return
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return // non-POSIX filesystem; nothing to do
	}
	uid, gid := int(sys.Uid), int(sys.Gid)
	if uid == os.Getuid() {
		return // already same owner (dev same-uid run); chown is unnecessary
	}
	if err := os.Chown(path, uid, gid); err != nil {
		log.Warn("could not chown the join token to the daemon user; it may be unreadable at boot",
			slog.String("path", path),
			slog.Int("uid", uid),
			slog.Int("gid", gid),
			slog.String("error", err.Error()),
			slog.String("manual_fix", "chown otherix:otherix "+path))
	}
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
