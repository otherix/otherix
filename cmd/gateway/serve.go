// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/gateway"
	"github.com/otherix/otherix/internal/logger"
	"github.com/otherix/otherix/internal/version"
)

// pollInterval is the State A polling cadence. Five seconds keeps
// re-bootstrap latency tight while avoiding journal spam over
// hours-long State A waits (error-string dedup suppresses repeated
// identical errors).
const pollInterval = 5 * time.Second

// gatewayCNPrefix is the literal prefix every gateway cert CommonName
// shares — `gateway-<name>` per internal/auth/csr.go SignGatewayCSR.
// parseGatewayNameFromCert strips it to surface the operator-supplied
// name.
const gatewayCNPrefix = "gateway-"

// newServeCommand builds the `otherix-gateway serve` subcommand. It boots
// into State A polling-loop mode, checking every 5s for cert + key + CA +
// config at the configured paths, and transitions to State B (gateway.Run)
// once all four are present and parseable.
func newServeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the gateway runtime (polls for cert material, transitions to State B)",
		Long: `Boots the gateway in State A polling-loop mode. Every 5 seconds the
loop checks if cert material (key + cert + CA) and the runtime config
exist and parse cleanly. Once all four are present the gateway
transitions to State B (full runtime — WireGuard mesh, overlay
datapath, heartbeat sender, CP-identity nudge endpoint).

The transition is one-way per process lifetime: a cert deleted mid-run
causes a heartbeat failure, not a return to State A. Operators restart
the gateway process to recover.

Run ` + "`otherix-gateway bootstrap`" + ` from an operator shell to
provision the cert material + config on a freshly-installed host.`,
		Args: cobra.NoArgs,
		RunE: runServe,
	}
	cmd.Flags().String("config", defaultConfigPath, "path to gateway.yaml")
	return cmd
}

func runServe(cmd *cobra.Command, _ []string) error {
	configPath, _ := cmd.Flags().GetString("config")
	showVersion, _ := cmd.Flags().GetBool("version")
	if showVersion {
		v := version.Current()
		fmt.Printf("otherix-%s %s (commit %s, built %s)\n", componentName, v.Version, v.Commit, v.Date)
		return nil
	}

	ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	bootLog := logger.WithComponent(
		logger.New(logger.Config{Level: "info", Format: "json"}),
		componentName,
	)
	logger.SetDefault(bootLog)
	bootLog.Info("gateway: serve starting",
		"binary", "otherix-"+componentName,
		"version", version.Current().Version,
		"config_path", configPath,
		"poll_interval", pollInterval.String(),
	)

	cfg, err := awaitReadyState(ctx, configPath, bootLog)
	if err != nil {
		return err
	}

	nodeName, err := parseGatewayNameFromCert(cfg.TLS.CertPath)
	if err != nil {
		return fmt.Errorf("parse gateway name from cert: %w", err)
	}

	runLog := logger.WithComponent(logger.New(cfg.Logger), componentName)
	logger.SetDefault(runLog)
	v := version.Current()
	runLog.Info("gateway: transitioning to State B",
		"binary", "otherix-"+componentName,
		"version", v.Version,
		"commit", v.Commit,
		"node_name", nodeName,
		"listen", cfg.Server.Listen,
		"control_plane_url", cfg.ControlPlane.URL,
	)

	if err := gateway.Run(ctx, cfg, nodeName, runLog); err != nil {
		runLog.Error("gateway stopped with error", "err", err)
		return err
	}
	runLog.Info("gateway: shutting down")
	return nil
}

// awaitReadyState implements the State A polling loop. Returns the loaded
// *config.AgentConfig once all cert files and the config yaml pass
// validation, or ctx.Err() if cancelled before readiness. First occurrence
// of an error string logs at WARN; identical retries stay silent.
func awaitReadyState(ctx context.Context, configPath string, log *slog.Logger) (*config.AgentConfig, error) {
	var lastErr string

	emitErr := func(err error) {
		s := err.Error()
		if s == lastErr {
			return
		}
		lastErr = s
		log.WarnContext(ctx, "gateway: waiting for cert material + config",
			slog.String("error", s),
			slog.String("config_path", configPath),
			slog.String("poll_interval", pollInterval.String()),
		)
	}

	if cfg, err := tryEnterStateB(configPath); err == nil {
		return cfg, nil
	} else {
		emitErr(err)
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
		cfg, err := tryEnterStateB(configPath)
		if err == nil {
			return cfg, nil
		}
		emitErr(err)
	}
}

// tryEnterStateB performs one pass of the State A → B readiness check:
// (1) config file exists and parses, (2) cert + key + CA files all exist
// at the paths the config declares. Returns the loaded
// *config.AgentConfig on success; a descriptive error on any failure.
func tryEnterStateB(configPath string) (*config.AgentConfig, error) {
	if _, err := os.Stat(configPath); err != nil {
		return nil, fmt.Errorf("config %s: %v", configPath, err)
	}
	cfg, err := config.LoadAgent(configPath)
	if err != nil {
		return nil, fmt.Errorf("load config: %v", err)
	}
	if err := requireFile("cert", cfg.TLS.CertPath); err != nil {
		return nil, err
	}
	if err := requireFile("key", cfg.TLS.KeyPath); err != nil {
		return nil, err
	}
	if err := requireFile("ca", cfg.TLS.CACertPath); err != nil {
		return nil, err
	}
	return cfg, nil
}

// requireFile returns nil if path resolves to an existing file; a
// descriptive error otherwise. Permission-denied is treated identically
// to not-found — the gateway process cannot use the file either way.
func requireFile(label, path string) error {
	if path == "" {
		return fmt.Errorf("%s_path is empty in config", label)
	}
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s %s: %v", label, path, err)
	}
	if st.IsDir() {
		return fmt.Errorf("%s %s is a directory, not a file", label, path)
	}
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("%s %s: permission denied", label, path)
	}
	return nil
}

// parseGatewayNameFromCert loads the gateway's own cert from certPath,
// parses the leaf, and returns the operator-supplied name encoded in the
// Subject CN. The cert template (internal/auth/csr.go SignGatewayCSR)
// emits CN `gateway-<name>`; this function strips the prefix. A `node-`
// CN (a hypervisor agent cert) is rejected — the binaries do not share
// identities.
func parseGatewayNameFromCert(certPath string) (string, error) {
	raw, err := os.ReadFile(certPath) //nolint:gosec // path is operator-configured (gateway.yaml tls.cert_path); the function exists to open this exact file
	if err != nil {
		return "", fmt.Errorf("read cert %s: %v", certPath, err)
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("cert %s: not a CERTIFICATE PEM block", certPath)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse cert %s: %v", certPath, err)
	}
	cn := strings.TrimSpace(cert.Subject.CommonName)
	if cn == "" {
		return "", fmt.Errorf("cert %s: Subject CommonName is empty", certPath)
	}
	if !strings.HasPrefix(cn, gatewayCNPrefix) {
		return "", fmt.Errorf("cert %s: Subject CN %q does not start with %q", certPath, cn, gatewayCNPrefix)
	}
	name := strings.TrimPrefix(cn, gatewayCNPrefix)
	if name == "" {
		return "", fmt.Errorf("cert %s: Subject CN %q has empty gateway name after prefix", certPath, cn)
	}
	return name, nil
}
