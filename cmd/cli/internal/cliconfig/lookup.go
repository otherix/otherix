// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cliconfig

import (
	"errors"
	"fmt"
)

// EnvToken и EnvServer name the env vars Resolve consults. The
// token var matches Phase C's existing OTHERIX_API_TOKEN; the
// server var is new — operators can pin а CP URL без editing
// the config file (typical CI: `OTHERIX_API_TOKEN=… OTHERIX_SERVER=…
// otherix vm list`).
const (
	EnvToken  = "OTHERIX_API_TOKEN" //nolint:gosec // env var name, not a credential
	EnvServer = "OTHERIX_SERVER"
)

// SourceFlag, SourceEnv и SourceConfig describe where Resolve
// picked the endpoint / token from. They surface в the
// ResolvedAuth.EndpointSource / TokenSource fields, used by `config
// show` и by log lines that help operators reason about precedence
// surprises.
const (
	SourceFlag   = "flag"
	SourceEnv    = "env"
	SourceConfig = "config"
)

// ResolvedAuth is the output of Resolve — endpoint + token ready
// to hand to cpclient.New. EndpointSource и TokenSource record
// where each piece came from (`flag`, `env`, `config:<cluster>`);
// they are not load-bearing for the network call but help operators
// debug "why is the CLI using this token".
type ResolvedAuth struct {
	Endpoint       string
	Token          string
	EndpointSource string
	TokenSource    string
	ClusterName    string
}

// ResolveOptions captures every input Resolve uses — populated by
// the caller from flags, env vars и a preloaded *Config. Callers
// should pass the empty string for any layer they want to skip.
type ResolveOptions struct {
	FlagEndpoint string
	FlagToken    string
	FlagCluster  string
	EnvEndpoint  string
	EnvToken     string
	Config       *Config
}

// ErrEndpointMissing signals that no precedence layer supplied a
// CP endpoint. The cobra wrapper translates this to "no endpoint
// configured: pass --endpoint, set OTHERIX_SERVER, or `otherix
// config use <cluster>`".
var ErrEndpointMissing = errors.New("cliconfig: no endpoint configured")

// ErrTokenMissing signals that no precedence layer supplied a
// bearer token. Surfaces к the user as "no token: pass --token,
// set OTHERIX_API_TOKEN, or `otherix config use <cluster>`".
var ErrTokenMissing = errors.New("cliconfig: no token configured")

// Resolve computes the (endpoint, token) pair the next CP call
// should use, applying the documented precedence chains:
//
// Endpoint (independent of token):
//
//	--endpoint flag → OTHERIX_SERVER → --cluster entry → current-cluster → fail
//
// Token (independent of endpoint):
//
//	--token flag → OTHERIX_API_TOKEN → --cluster entry → current-cluster → fail
//
// The two are intentionally independent — а typical CI/CD pattern
// is "env var token, flag endpoint" и each precedence chain
// shortcuts independently. When --cluster is set, only that
// cluster is consulted на the config layer (NOT current-cluster as
// fallback); this matches kubectl's --context behaviour.
//
// A nil opts.Config behaves как an empty config — Resolve still
// works против flags + env alone, which is the Phase C smoke-test
// path и must not regress.
func Resolve(opts ResolveOptions) (ResolvedAuth, error) {
	out := ResolvedAuth{}

	cluster, err := selectCluster(opts)
	if err != nil {
		return out, err
	}
	if cluster != nil {
		out.ClusterName = cluster.Name
	}

	switch {
	case opts.FlagEndpoint != "":
		out.Endpoint = opts.FlagEndpoint
		out.EndpointSource = SourceFlag
	case opts.EnvEndpoint != "":
		out.Endpoint = opts.EnvEndpoint
		out.EndpointSource = SourceEnv
	case cluster != nil && cluster.Server != "":
		out.Endpoint = cluster.Server
		out.EndpointSource = fmt.Sprintf("%s:%s", SourceConfig, cluster.Name)
	default:
		return out, ErrEndpointMissing
	}

	switch {
	case opts.FlagToken != "":
		out.Token = opts.FlagToken
		out.TokenSource = SourceFlag
	case opts.EnvToken != "":
		out.Token = opts.EnvToken
		out.TokenSource = SourceEnv
	case cluster != nil && cluster.Token != "":
		out.Token = cluster.Token
		out.TokenSource = fmt.Sprintf("%s:%s", SourceConfig, cluster.Name)
	default:
		return out, ErrTokenMissing
	}

	return out, nil
}

// selectCluster picks the config cluster for Resolve: --cluster
// flag wins (and surfaces ErrClusterNotFound if missing — never
// silently falls through); otherwise current-cluster. Returns
// (nil, nil) when neither names а cluster — that is а valid state
// (flags + env alone may carry the full credential pair).
func selectCluster(opts ResolveOptions) (*Cluster, error) {
	if opts.Config == nil {
		if opts.FlagCluster != "" {
			return nil, fmt.Errorf("%w: %q (no config loaded)", ErrClusterNotFound, opts.FlagCluster)
		}
		return nil, nil
	}
	if opts.FlagCluster != "" {
		return opts.Config.FindCluster(opts.FlagCluster)
	}
	if opts.Config.CurrentCluster == "" {
		return nil, nil
	}
	return opts.Config.FindCluster(opts.Config.CurrentCluster)
}
