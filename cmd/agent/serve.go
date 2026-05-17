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
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/internal/agent"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/logger"
	"github.com/otherix/otherix/internal/version"
)

// pollInterval is the State A polling cadence. Five seconds keeps
// re-bootstrap latency tight while avoiding journal spam over
// hours-long State A waits (error-string dedup suppresses repeated
// identical errors).
const pollInterval = 5 * time.Second

// newServeCommand builds the `otherix-agent serve` subcommand. The
// implementation IS the polling-loop reframe: enter State A,
// every 5s check для cert + key + CA + config files at expected paths
// (per cfg.TLS / --config); transition к State B (agent.Run) once all
// four are present и parseable. Validation failures stay в State A с
// error-string-dedup logging.
func newServeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the agent runtime (polls для cert material, transitions к State B)",
		Long: `Boots the agent в State A polling-loop mode. Every 5 seconds the
loop checks if cert material (key + cert + CA) и the runtime config
exist и parse cleanly. Once все four are present the agent
transitions к State B (full runtime — HTTPS mTLS server + heartbeat
sender).

The transition is one-way per process lifetime (SL4): а cert deleted
mid-run causes а heartbeat failure, not а return к State A. Operators
restart the agent process к recover.

Run ` + "`otherix-agent bootstrap`" + ` от an operator shell к
provision the cert material + config on а freshly-installed host.`,
		Args: cobra.NoArgs,
		RunE: runServe,
	}
	cmd.Flags().String("config", defaultConfigPath, "path to agent.yaml")
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

	// Bootstrap log — uses а minimal default config until we successfully
	// load the on-disk yaml. Once State B fires the agent reinitialises
	// its logger от the loaded cfg.Logger.
	bootLog := logger.WithComponent(
		logger.New(logger.Config{Level: "info", Format: "json"}),
		componentName,
	)
	logger.SetDefault(bootLog)
	bootLog.Info("agent: serve starting",
		"binary", "otherix-"+componentName,
		"version", version.Current().Version,
		"config_path", configPath,
		"poll_interval", pollInterval.String(),
	)

	cfg, err := awaitReadyState(ctx, configPath, bootLog)
	if err != nil {
		return err
	}

	runLog := logger.WithComponent(logger.New(cfg.Logger), componentName)
	logger.SetDefault(runLog)
	v := version.Current()
	runLog.Info("agent: transitioning к State B",
		"binary", "otherix-"+componentName,
		"version", v.Version,
		"commit", v.Commit,
		"listen", cfg.Server.Listen,
		"control_plane_url", cfg.ControlPlane.URL,
	)

	if err := agent.Run(ctx, cfg, runLog); err != nil {
		runLog.Error("agent stopped с error", "err", err)
		return err
	}
	runLog.Info("agent: shutting down")
	return nil
}

// awaitReadyState implements the State A polling loop. Returns the
// loaded *config.AgentConfig once все cert files и the config yaml
// pass validation. Returns ctx.Err() если the context is cancelled
// before readiness (clean shutdown signal during boot).
//
// Logging strategy per SL3: first occurrence of an error string logs
// at WARN; identical retries stay silent. А different error logs
// fresh; success logs once at the transition. Avoids journal spam
// over hours-long State A waits (failed bootstrap, missing config,
// expired cert).
func awaitReadyState(ctx context.Context, configPath string, log *slog.Logger) (*config.AgentConfig, error) {
	var lastErr string

	emitErr := func(err error) {
		s := err.Error()
		if s == lastErr {
			return
		}
		lastErr = s
		log.WarnContext(ctx, "agent: waiting для cert material + config",
			slog.String("error", s),
			slog.String("config_path", configPath),
			slog.String("poll_interval", pollInterval.String()),
		)
	}

	// Try once immediately, then loop on ticks. Avoids а stray 5s
	// sleep on already-bootstrapped hosts.
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
// (1) config file exists и parses, (2) cert + key + CA files all exist
// at the paths the config declares, (3) cert is loadable (PEM well-formed,
// keypair matches). Returns the loaded *config.AgentConfig on success;
// а descriptive error on any failure.
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

// requireFile returns nil если path resolves к an existing file; а
// descriptive error otherwise. Permission-denied is treated identically
// к not-found — the agent process cannot use the file either way.
func requireFile(label, path string) error {
	if path == "" {
		return fmt.Errorf("%s_path is empty in config", label)
	}
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s %s: %v", label, path, err)
	}
	if st.IsDir() {
		return fmt.Errorf("%s %s is а directory, not а file", label, path)
	}
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("%s %s: permission denied", label, path)
	}
	return nil
}
