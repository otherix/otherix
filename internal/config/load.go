// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package config

import (
	"fmt"
	"strings"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

const envPrefix = "OTHERIX_"

// load reads the optional YAML file at path, then overlays OTHERIX_-prefixed
// environment variables, and finally unmarshals the result into cfg. cfg must
// be a non-nil pointer to a config struct with koanf tags. Nesting separator
// inside env var names is __ (double underscore) because single underscores
// appear in snake_case key names. Errors are formatted with %v — callers do
// not inspect them with errors.Is/As.
func load(path string, cfg any) error {
	k := koanf.New(".")

	if path != "" {
		if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
			return fmt.Errorf("read config %q: %v", path, err)
		}
	}

	envProvider := env.Provider(envPrefix, ".", func(s string) string {
		return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(s, envPrefix)), "__", ".")
	})
	if err := k.Load(envProvider, nil); err != nil {
		return fmt.Errorf("read env: %v", err)
	}

	if err := k.UnmarshalWithConf("", cfg, koanf.UnmarshalConf{Tag: "koanf"}); err != nil {
		return fmt.Errorf("decode config: %v", err)
	}
	return nil
}

// validator is implemented by every per-binary config type. The var below is a
// compile-time guard: removing Validate from any listed type breaks the build
// rather than silently skipping validation at startup.
type validator interface{ Validate() error }

var _ = []validator{APIConfig{}, AgentConfig{}}

// LoadAPI loads the otherix-api configuration: defaults overlaid with the YAML
// at path (if non-empty) overlaid with OTHERIX_-prefixed environment
// variables, then validated.
func LoadAPI(path string) (*APIConfig, error) {
	cfg := defaultAPIConfig()
	if err := load(path, &cfg); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate api config: %v", err)
	}
	return &cfg, nil
}

// LoadAgent loads the otherix-agent configuration. See LoadAPI for the
// layering rules.
func LoadAgent(path string) (*AgentConfig, error) {
	cfg := defaultAgentConfig()
	if err := load(path, &cfg); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate agent config: %v", err)
	}
	return &cfg, nil
}
