// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cliconfig_test

import (
	"errors"
	"testing"

	"github.com/otherix/otherix/cmd/cli/internal/cliconfig"
)

// TestResolve_Matrix walks the precedence truth-table. Each case
// fixes inputs at every layer и asserts which layer Resolve picked.
// Failure messages quote the row name so a failure points straight
// at the broken case.
func TestResolve_Matrix(t *testing.T) {
	cfg := &cliconfig.Config{
		CurrentCluster: "prod",
		Clusters: []cliconfig.Cluster{
			{Name: "prod", Server: "https://prod", Token: "otx_prod"},
			{Name: "stg", Server: "http://stg", Token: "otx_stg"},
		},
	}

	type row struct {
		name           string
		opts           cliconfig.ResolveOptions
		wantEndpoint   string
		wantTokenSrc   string
		wantEndptSrc   string
		wantCluster    string
		wantTokenValue string
	}
	cases := []row{
		{
			name: "flag_token_flag_endpoint",
			opts: cliconfig.ResolveOptions{
				FlagEndpoint: "http://flag", FlagToken: "tok_flag",
				EnvToken: "tok_env", EnvEndpoint: "http://env",
				Config: cfg,
			},
			wantEndpoint: "http://flag", wantEndptSrc: cliconfig.SourceFlag,
			wantTokenValue: "tok_flag", wantTokenSrc: cliconfig.SourceFlag,
			wantCluster: "prod",
		},
		{
			name: "env_token_flag_endpoint",
			opts: cliconfig.ResolveOptions{
				FlagEndpoint: "http://flag",
				EnvToken:     "tok_env", EnvEndpoint: "http://env",
				Config: cfg,
			},
			wantEndpoint: "http://flag", wantEndptSrc: cliconfig.SourceFlag,
			wantTokenValue: "tok_env", wantTokenSrc: cliconfig.SourceEnv,
			wantCluster: "prod",
		},
		{
			name: "env_token_env_endpoint",
			opts: cliconfig.ResolveOptions{
				EnvToken: "tok_env", EnvEndpoint: "http://env",
				Config: cfg,
			},
			wantEndpoint: "http://env", wantEndptSrc: cliconfig.SourceEnv,
			wantTokenValue: "tok_env", wantTokenSrc: cliconfig.SourceEnv,
			wantCluster: "prod",
		},
		{
			name: "config_current_only",
			opts: cliconfig.ResolveOptions{Config: cfg},
			// Current-cluster is "prod" — both values come from it.
			wantEndpoint: "https://prod", wantEndptSrc: "config:prod",
			wantTokenValue: "otx_prod", wantTokenSrc: "config:prod",
			wantCluster: "prod",
		},
		{
			name:         "explicit_cluster_flag",
			opts:         cliconfig.ResolveOptions{FlagCluster: "stg", Config: cfg},
			wantEndpoint: "http://stg", wantEndptSrc: "config:stg",
			wantTokenValue: "otx_stg", wantTokenSrc: "config:stg",
			wantCluster: "stg",
		},
		{
			name:         "mixed_env_token_config_endpoint",
			opts:         cliconfig.ResolveOptions{EnvToken: "tok_env", Config: cfg},
			wantEndpoint: "https://prod", wantEndptSrc: "config:prod",
			wantTokenValue: "tok_env", wantTokenSrc: cliconfig.SourceEnv,
			wantCluster: "prod",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cliconfig.Resolve(tc.opts)
			if err != nil {
				t.Fatalf("Resolve(%s) error = %v", tc.name, err)
			}
			if got.Endpoint != tc.wantEndpoint {
				t.Errorf("Endpoint = %q, want %q", got.Endpoint, tc.wantEndpoint)
			}
			if got.EndpointSource != tc.wantEndptSrc {
				t.Errorf("EndpointSource = %q, want %q", got.EndpointSource, tc.wantEndptSrc)
			}
			if got.Token != tc.wantTokenValue {
				t.Errorf("Token = %q, want %q", got.Token, tc.wantTokenValue)
			}
			if got.TokenSource != tc.wantTokenSrc {
				t.Errorf("TokenSource = %q, want %q", got.TokenSource, tc.wantTokenSrc)
			}
			if got.ClusterName != tc.wantCluster {
				t.Errorf("ClusterName = %q, want %q", got.ClusterName, tc.wantCluster)
			}
		})
	}
}

func TestResolve_EndpointMissing(t *testing.T) {
	_, err := cliconfig.Resolve(cliconfig.ResolveOptions{
		FlagToken: "tok",
	})
	if !errors.Is(err, cliconfig.ErrEndpointMissing) {
		t.Errorf("err = %v, want ErrEndpointMissing", err)
	}
}

func TestResolve_TokenMissing(t *testing.T) {
	_, err := cliconfig.Resolve(cliconfig.ResolveOptions{
		FlagEndpoint: "http://x",
	})
	if !errors.Is(err, cliconfig.ErrTokenMissing) {
		t.Errorf("err = %v, want ErrTokenMissing", err)
	}
}

func TestResolve_FlagClusterUnknown(t *testing.T) {
	cfg := &cliconfig.Config{
		CurrentCluster: "prod",
		Clusters:       []cliconfig.Cluster{{Name: "prod", Server: "u", Token: "t"}},
	}
	_, err := cliconfig.Resolve(cliconfig.ResolveOptions{
		FlagCluster: "ghost",
		Config:      cfg,
	})
	if !errors.Is(err, cliconfig.ErrClusterNotFound) {
		t.Errorf("err = %v, want ErrClusterNotFound", err)
	}
}

func TestResolve_NilConfigWithFlagClusterErrors(t *testing.T) {
	_, err := cliconfig.Resolve(cliconfig.ResolveOptions{
		FlagCluster: "anything",
	})
	if !errors.Is(err, cliconfig.ErrClusterNotFound) {
		t.Errorf("err = %v, want ErrClusterNotFound", err)
	}
}

func TestResolve_NilConfigFlagsAlone(t *testing.T) {
	got, err := cliconfig.Resolve(cliconfig.ResolveOptions{
		FlagEndpoint: "http://x",
		FlagToken:    "t",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Endpoint != "http://x" || got.Token != "t" {
		t.Errorf("flags-only resolve: %+v", got)
	}
}
