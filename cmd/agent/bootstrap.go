// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/internal/agent/bootstrap"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/logger"
)

const (
	// defaultCertDir is the on-disk directory the bootstrap protocol
	// targets for cert material per Otherix filesystem convention
	// (`/opt/otherix/` for runtime state, `/etc/otherix/` for operator
	// config).
	defaultCertDir = "/opt/otherix/certs"

	// defaultMigrationPortStart / defaultMigrationPortEnd mirror the
	// ephemeral-range default. Operators may override via
	// --migration-port-range-start / --migration-port-range-end.
	defaultMigrationPortStart = 49152
	defaultMigrationPortEnd   = 49251

	// defaultListenAddr is the bind address baked into the generated
	// agent-config.yml when --listen is omitted. 0.0.0.0:9443 matches
	// the existing dev configs.
	defaultListenAddr = "0.0.0.0:9443"

	// defaultHeartbeatInterval is the per-tick cadence baked into the
	// generated agent-config.yml when --heartbeat-interval is omitted.
	defaultHeartbeatInterval = 30 * time.Second
)

// newBootstrapCommand builds the `otherix-agent bootstrap` subcommand.
// One-shot: invoke from an operator shell on a freshly-installed host,
// it executes the join-token protocol and writes cert material + config
// to disk. The running `serve` loop picks up the new files on the next
// 5s tick.
func newBootstrapCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Provision cert material + agent-config.yml on a fresh host",
		Long: `One-shot bootstrap subcommand. Reads operator-supplied flags,
executes the join-token protocol against the CP, and writes the issued
cert material plus a generated agent-config.yml to disk. Idempotent — a
repeat invocation without --force on an already-bootstrapped host
exits 0 with a "already bootstrapped" message.

The agent's serve loop polls for these files every 5 seconds, so
bootstrap completion automatically unblocks the runtime — no service
restart needed (a partial state on disk without cert material is what
keeps serve in State A; bootstrap writes the missing pieces atomically).

Examples:
  # Token literal:
  otherix-agent bootstrap \
    --token=otx_join_... --ca-fingerprint=sha256:... \
    --cp-url=https://cp.example:8443 \
    --node-name=node-mvp \
    --advertised-endpoint=https://127.0.0.1:9443 \
    --migration-host=0.0.0.0

  # Token from file:
  otherix-agent bootstrap --token-path=/etc/otherix/bootstrap-token ...

  # Token from env var (resolves at invocation):
  OTX_TOKEN=otx_join_... otherix-agent bootstrap --token-env=OTX_TOKEN ...`,
		Args: cobra.NoArgs,
		RunE: runBootstrap,
	}
	flags := cmd.Flags()
	// Token sources (L7 — exactly one must be set).
	flags.String("token", "", "join token plaintext (mutually exclusive with --token-path / --token-env)")
	flags.String("token-path", "", "path to file holding the join token (whitespace-trimmed)")
	flags.String("token-env", "", "name of env var holding the join token (resolved at invocation)")
	// Required identity / endpoint inputs.
	flags.String("ca-fingerprint", "", "cluster CA sha256 fingerprint (sha256:<hex> or bare hex)")
	flags.String("cp-url", "", "control-plane base URL (https://...)")
	flags.String("node-name", "", "cluster-unique node name")
	flags.String("advertised-endpoint", "", "HTTPS URL the CP uses to reach the agent")
	flags.String("migration-host", "", "hostname/IP the agent advertises for migration ingress (L14)")
	// Optional knobs.
	flags.Int("migration-port-range-start", defaultMigrationPortStart, "migration port range lower bound")
	flags.Int("migration-port-range-end", defaultMigrationPortEnd, "migration port range upper bound")
	flags.String("listen", defaultListenAddr, "agent HTTPS bind address (baked into agent-config.yml)")
	flags.Duration("heartbeat-interval", defaultHeartbeatInterval, "heartbeat cadence (baked into agent-config.yml)")
	flags.String("cert-dir", defaultCertDir, "directory for cert material (key/cert/CA atomic writes)")
	flags.String("config-path", defaultConfigPath, "destination for the generated agent-config.yml")
	flags.Bool("force", false, "overwrite existing cert material + config (re-bootstrap)")
	flags.Duration("request-timeout", 30*time.Second, "per-HTTP-request timeout against the CP")
	return cmd
}

// bootstrapInputs is the validated CLI flag bundle, derived once at
// runBootstrap entry. Drives both idempotency checking and the
// `bootstrap.Bootstrap()` library call. Architecture comes from
// runtime.GOARCH per L10.
type bootstrapInputs struct {
	token                   string
	caFingerprint           string
	cpURL                   string
	nodeName                string
	advertisedEndpoint      string
	migrationHost           string
	migrationPortRangeStart int
	migrationPortRangeEnd   int
	listenAddr              string
	heartbeatInterval       time.Duration
	certDir                 string
	configPath              string
	force                   bool
	requestTimeout          time.Duration
}

func runBootstrap(cmd *cobra.Command, _ []string) error {
	in, err := readBootstrapInputs(cmd)
	if err != nil {
		return err
	}

	log := logger.WithComponent(
		logger.New(logger.Config{Level: "info", Format: "json"}),
		componentName,
	)

	keyPath, certPath, caPath := certPaths(in.certDir)

	// Idempotency is scoped to cert material only (key + cert + CA).
	// The config file (agent-config.yml) is preserved if it already
	// exists — operators commonly hand-tune logger / pools / etc., and
	// blindly overwriting that on every bootstrap would erase work.
	// --force does overwrite both cert material and config.
	if !in.force {
		switch state := inspectCertState(keyPath, certPath, caPath); state {
		case stateAllPresent:
			log.Info("agent: already bootstrapped — cert material present (use --force to re-bootstrap)",
				slog.String("cert_path", certPath),
				slog.String("key_path", keyPath),
				slog.String("ca_path", caPath),
			)
			return nil
		case statePartial:
			return fmt.Errorf("partial cert material on disk — some of (%s, %s, %s) present but not all; delete the partial files or re-run with --force",
				keyPath, certPath, caPath)
		case stateClean:
			// fall through to bootstrap protocol
		}
	}

	ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cfg := &config.BootstrapConfig{
		Token:                   in.token,
		CAFingerprint:           in.caFingerprint,
		CPURL:                   in.cpURL,
		NodeName:                in.nodeName,
		Architecture:            runtime.GOARCH,
		AdvertisedEndpoint:      in.advertisedEndpoint,
		MigrationHost:           in.migrationHost,
		MigrationPortRangeStart: in.migrationPortRangeStart,
		MigrationPortRangeEnd:   in.migrationPortRangeEnd,
		RequestTimeout:          in.requestTimeout,
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validate bootstrap inputs: %v", err)
	}

	log.InfoContext(ctx, "agent: bootstrap starting",
		slog.String("cp_url", cfg.CPURL),
		slog.String("node_name", cfg.NodeName),
		slog.String("architecture", cfg.Architecture),
	)

	result, err := bootstrap.Bootstrap(ctx, cfg, log)
	if err != nil {
		return fmt.Errorf("bootstrap protocol: %w", err)
	}

	if err := bootstrap.Persist(certPath, keyPath, caPath, result); err != nil {
		return fmt.Errorf("persist bootstrap result (token CONSUMED at CP — cert material in-memory only, manual recovery required): %w", err)
	}

	// Config write: idempotent — bootstrap writes the agent runtime
	// config only when absent. --force re-issues cert material but
	// never overwrites the config, so operator-tuned settings (logger,
	// pools, etc.) survive across re-bootstraps. Fresh deployments
	// still get the baseline yaml automatically.
	wroteConfig := false
	if !fileExists(in.configPath) {
		if err := writeAgentConfig(in.configPath, agentConfigInputs{
			CPURL:                   in.cpURL,
			HeartbeatInterval:       in.heartbeatInterval,
			ListenAddr:              in.listenAddr,
			CertPath:                certPath,
			KeyPath:                 keyPath,
			CAPath:                  caPath,
			MigrationHost:           in.migrationHost,
			MigrationPortRangeStart: in.migrationPortRangeStart,
			MigrationPortRangeEnd:   in.migrationPortRangeEnd,
		}); err != nil {
			return fmt.Errorf("write agent-config.yml: %w", err)
		}
		wroteConfig = true
	}

	log.InfoContext(ctx, "agent: bootstrap complete",
		slog.String("node_id", result.NodeID),
		slog.String("node_name", cfg.NodeName),
		slog.String("cert_path", certPath),
		slog.String("config_path", in.configPath),
		slog.Bool("config_written", wroteConfig),
	)
	return nil
}

// readBootstrapInputs gathers flags, resolves the token via L7's three-
// way --token / --token-path / --token-env mux, and rejects invalid
// combinations. Returns a fully-formed bootstrapInputs ready for use.
func readBootstrapInputs(cmd *cobra.Command) (bootstrapInputs, error) {
	flags := cmd.Flags()
	tokenLit, _ := flags.GetString("token")
	tokenPath, _ := flags.GetString("token-path")
	tokenEnv, _ := flags.GetString("token-env")
	token, err := resolveTokenFromFlags(tokenLit, tokenPath, tokenEnv)
	if err != nil {
		return bootstrapInputs{}, err
	}

	in := bootstrapInputs{}
	in.token = token
	in.caFingerprint, _ = flags.GetString("ca-fingerprint")
	in.cpURL, _ = flags.GetString("cp-url")
	in.nodeName, _ = flags.GetString("node-name")
	in.advertisedEndpoint, _ = flags.GetString("advertised-endpoint")
	in.migrationHost, _ = flags.GetString("migration-host")
	in.migrationPortRangeStart, _ = flags.GetInt("migration-port-range-start")
	in.migrationPortRangeEnd, _ = flags.GetInt("migration-port-range-end")
	in.listenAddr, _ = flags.GetString("listen")
	in.heartbeatInterval, _ = flags.GetDuration("heartbeat-interval")
	in.certDir, _ = flags.GetString("cert-dir")
	in.configPath, _ = flags.GetString("config-path")
	in.force, _ = flags.GetBool("force")
	in.requestTimeout, _ = flags.GetDuration("request-timeout")
	return in, nil
}

// resolveTokenFromFlags returns the plaintext token, applying the L7
// three-way mux. Whitespace is trimmed from path/env reads.
func resolveTokenFromFlags(tokenLit, tokenPath, tokenEnv string) (string, error) {
	sources := 0
	if tokenLit != "" {
		sources++
	}
	if tokenPath != "" {
		sources++
	}
	if tokenEnv != "" {
		sources++
	}
	switch sources {
	case 0:
		return "", errors.New("one of --token / --token-path / --token-env is required")
	case 1:
		// ok
	default:
		return "", errors.New("--token / --token-path / --token-env are mutually exclusive — specify exactly one")
	}
	if tokenLit != "" {
		return tokenLit, nil
	}
	if tokenPath != "" {
		raw, err := os.ReadFile(tokenPath) //nolint:gosec // operator-supplied path
		if err != nil {
			return "", fmt.Errorf("read token-path %s: %v", tokenPath, err)
		}
		s := strings.TrimSpace(string(raw))
		if s == "" {
			return "", fmt.Errorf("token-path %s is empty after trimming whitespace", tokenPath)
		}
		return s, nil
	}
	// tokenEnv path.
	s := strings.TrimSpace(os.Getenv(tokenEnv))
	if s == "" {
		return "", fmt.Errorf("token-env %q is unset or empty", tokenEnv)
	}
	return s, nil
}

// presenceState captures the on-disk state of the four bootstrap output
// files. Used by the idempotency check.
type presenceState int

const (
	stateClean presenceState = iota
	statePartial
	stateAllPresent
)

// inspectCertState reports whether the three cert material files are
// all-present, all-absent, or partial. The polling-loop reframe
// requires all three to be present (alongside the runtime config) for
// State B; partial without --force is rejected at the API edge here
// per SL6.
//
// The config file is intentionally excluded — it is preserved across
// re-bootstraps by default; --force overrides.
func inspectCertState(keyPath, certPath, caPath string) presenceState {
	present := 0
	for _, p := range []string{keyPath, certPath, caPath} {
		if fileExists(p) {
			present++
		}
	}
	switch present {
	case 0:
		return stateClean
	case 3:
		return stateAllPresent
	default:
		return statePartial
	}
}

// certPaths derives the (key, cert, CA) destination paths from --cert-dir.
// File names match the convention in dev/config/agent-*.yaml so the
// generated agent-config.yml points at the right files without operator
// intervention.
func certPaths(certDir string) (keyPath, certPath, caPath string) {
	return filepath.Join(certDir, "agent.key"),
		filepath.Join(certDir, "agent.crt"),
		filepath.Join(certDir, "ca.crt")
}

// fileExists treats permission-denied identically to not-found — the
// agent runtime would fail to use the file either way.
func fileExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// agentConfigInputs is the data the agent-config.yml template needs.
// Kept narrow so the template stays decoupled from the larger bootstrapInputs.
type agentConfigInputs struct {
	CPURL                   string
	HeartbeatInterval       time.Duration
	ListenAddr              string
	CertPath                string
	KeyPath                 string
	CAPath                  string
	MigrationHost           string
	MigrationPortRangeStart int
	MigrationPortRangeEnd   int
}

// _ ensures context import stays used when the file evolves; placeholder
// removed once additional ctx-aware helpers land.
var _ = context.Background
