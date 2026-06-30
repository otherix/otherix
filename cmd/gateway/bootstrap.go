// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
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
	// (`/var/lib/otherix/` for runtime state, `/etc/otherix/` for operator
	// config).
	defaultCertDir = "/var/lib/otherix/certs"

	// defaultListenAddr is the bind address baked into the generated
	// gateway.yaml when --listen is omitted. The gateway listener serves
	// the CP-identity control endpoints (heartbeat nudge, the future
	// connect/splice route) over mTLS.
	defaultListenAddr = "0.0.0.0:9443"

	// defaultHeartbeatInterval is the per-tick cadence baked into the
	// generated gateway.yaml when --heartbeat-interval is omitted.
	defaultHeartbeatInterval = 30 * time.Second
)

// newBootstrapCommand builds the `otherix-gateway bootstrap` subcommand.
// One-shot: invoke from an operator shell on a freshly-installed host, it
// executes the gateway join-token protocol and writes cert material +
// config to disk. The running `serve` loop picks up the new files on the
// next 5s tick.
func newBootstrapCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Provision cert material + gateway.yaml on a fresh host",
		Long: `One-shot bootstrap subcommand. Reads operator-supplied flags,
executes the gateway join-token protocol against the CP, and writes the
issued cert material plus a generated gateway.yaml to disk. Idempotent —
a repeat invocation without --force on an already-bootstrapped host
exits 0 with a "already bootstrapped" message.

The gateway's serve loop polls for these files every 5 seconds, so
bootstrap completion automatically unblocks the runtime — no service
restart needed.

Examples:
  otherix-gateway bootstrap \
    --token=otx_join_... --ca-fingerprint=sha256:... \
    --cp-url=https://cp.example:8443 \
    --node-name=edge1 \
    --advertised-endpoint=https://203.0.113.7:9443`,
		Args: cobra.NoArgs,
		RunE: runBootstrap,
	}
	flags := cmd.Flags()
	// Token sources (exactly one must be set).
	flags.String("token", "", "join token plaintext (mutually exclusive with --token-path / --token-env)")
	flags.String("token-path", "", "path to file holding the join token (whitespace-trimmed)")
	flags.String("token-env", "", "name of env var holding the join token (resolved at invocation)")
	// Required identity / endpoint inputs.
	flags.String("ca-fingerprint", "", "cluster CA sha256 fingerprint (sha256:<hex> or bare hex)")
	flags.String("cp-url", "", "control-plane base URL (https://...)")
	flags.String("node-name", "", "cluster-unique gateway name")
	flags.String("advertised-endpoint", "", "HTTPS URL the CP uses to reach the gateway")
	// Optional knobs.
	flags.String("listen", defaultListenAddr, "gateway HTTPS bind address (baked into gateway.yaml)")
	flags.Duration("heartbeat-interval", defaultHeartbeatInterval, "heartbeat cadence (baked into gateway.yaml)")
	flags.String("cert-dir", defaultCertDir, "directory for cert material (key/cert/CA atomic writes)")
	flags.String("config-path", defaultConfigPath, "destination for the generated gateway.yaml")
	flags.Bool("force", false, "overwrite existing cert material (re-bootstrap); never overwrites an existing config")
	flags.Duration("request-timeout", 30*time.Second, "per-HTTP-request timeout against the CP")
	return cmd
}

// bootstrapInputs is the validated CLI flag bundle, derived once at
// runBootstrap entry. Architecture comes from runtime.GOARCH.
type bootstrapInputs struct {
	token              string
	caFingerprint      string
	cpURL              string
	nodeName           string
	advertisedEndpoint string
	listenAddr         string
	heartbeatInterval  time.Duration
	certDir            string
	configPath         string
	force              bool
	requestTimeout     time.Duration
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

	// Idempotency is scoped to cert material only (key + cert + CA). The
	// config file (gateway.yaml) is preserved if it already exists so
	// operator-tuned settings survive a re-bootstrap; --force overwrites
	// cert material but never the config.
	if !in.force {
		switch state := inspectCertState(keyPath, certPath, caPath); state {
		case stateAllPresent:
			log.Info("gateway: already bootstrapped — cert material present (use --force to re-bootstrap)",
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

	// The migration fields are inert for a gateway (it hosts no VMs and is
	// never a migration target), but the shared join protocol validates
	// them, so supply harmless defaults. The generated gateway.yaml omits
	// the migration block entirely.
	cfg := &config.BootstrapConfig{
		Token:                   in.token,
		CAFingerprint:           in.caFingerprint,
		CPURL:                   in.cpURL,
		NodeName:                in.nodeName,
		Architecture:            runtime.GOARCH,
		AdvertisedEndpoint:      in.advertisedEndpoint,
		MigrationHost:           "0.0.0.0",
		MigrationPortRangeStart: 49152,
		MigrationPortRangeEnd:   49251,
		RequestTimeout:          in.requestTimeout,
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validate bootstrap inputs: %v", err)
	}

	log.InfoContext(ctx, "gateway: bootstrap starting",
		slog.String("cp_url", cfg.CPURL),
		slog.String("node_name", cfg.NodeName),
		slog.String("architecture", cfg.Architecture),
	)

	result, err := bootstrap.Gateway(ctx, cfg, log)
	if err != nil {
		return fmt.Errorf("bootstrap protocol: %w", err)
	}

	if err := bootstrap.Persist(certPath, keyPath, caPath, result); err != nil {
		return fmt.Errorf("persist bootstrap result (token CONSUMED at CP — cert material in-memory only, manual recovery required): %w", err)
	}

	// Config write: idempotent — bootstrap writes the runtime config only
	// when absent, so operator-tuned settings survive a re-bootstrap.
	wroteConfig := false
	if !fileExists(in.configPath) {
		if err := writeGatewayConfig(in.configPath, gatewayConfigInputs{
			CPURL:             in.cpURL,
			HeartbeatInterval: in.heartbeatInterval,
			ListenAddr:        in.listenAddr,
			CertPath:          certPath,
			KeyPath:           keyPath,
			CAPath:            caPath,
		}); err != nil {
			return fmt.Errorf("write gateway.yaml: %w", err)
		}
		wroteConfig = true
	}

	log.InfoContext(ctx, "gateway: bootstrap complete",
		slog.String("node_id", result.NodeID),
		slog.String("node_name", cfg.NodeName),
		slog.String("cert_path", certPath),
		slog.String("config_path", in.configPath),
		slog.Bool("config_written", wroteConfig),
	)
	return nil
}

// readBootstrapInputs gathers flags and resolves the token via the three-
// way --token / --token-path / --token-env mux.
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
	in.listenAddr, _ = flags.GetString("listen")
	in.heartbeatInterval, _ = flags.GetDuration("heartbeat-interval")
	in.certDir, _ = flags.GetString("cert-dir")
	in.configPath, _ = flags.GetString("config-path")
	in.force, _ = flags.GetBool("force")
	in.requestTimeout, _ = flags.GetDuration("request-timeout")
	return in, nil
}

// resolveTokenFromFlags returns the plaintext token, applying the three-
// way mux. Whitespace is trimmed from path/env reads.
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
	s := strings.TrimSpace(os.Getenv(tokenEnv))
	if s == "" {
		return "", fmt.Errorf("token-env %q is unset or empty", tokenEnv)
	}
	return s, nil
}

// presenceState captures the on-disk state of the three cert material
// files. Used by the idempotency check.
type presenceState int

const (
	stateClean presenceState = iota
	statePartial
	stateAllPresent
)

// inspectCertState reports whether the three cert material files are
// all-present, all-absent, or partial. The config file is intentionally
// excluded — it is preserved across re-bootstraps by default.
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
func certPaths(certDir string) (keyPath, certPath, caPath string) {
	return filepath.Join(certDir, "gateway.key"),
		filepath.Join(certDir, "gateway.crt"),
		filepath.Join(certDir, "ca.crt")
}

// fileExists treats permission-denied identically to not-found — the
// gateway runtime would fail to use the file either way.
func fileExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}
