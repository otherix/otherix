// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cluster

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// newJoinTokenCreateCommand builds `otherix cluster join-token create`.
// Mints a kind=cluster token bundle and prints it exactly once.
func newJoinTokenCreateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Mint a cluster-replica join token (admin only).",
		Long: `Mints a fresh kind=cluster join token. Output is the token plaintext
plus the cluster CA fingerprint, returned exactly once. Hand BOTH to a
new control-plane host:

  sudo otherix-api join --cp-url https://<this-cp>:8080 \
    --token <token> --ca-fingerprint sha256:<fingerprint> --name <unique>

A cluster token redeems for the CA private key, so it defaults to
single-use; pass --max-uses N to grow several replicas with one token.`,
		RunE: runJoinTokenCreate,
	}
	cmd.Flags().Duration(flagTTL, defaultTokenTTL, "token validity duration (1m..24h)")
	cmd.Flags().Int32(flagMaxUses, 0, "consumption cap (0 = server default of 1 for cluster tokens)")
	cmd.Flags().String(flagOutput, "text", "output format: text|json")
	return cmd
}

func runJoinTokenCreate(cmd *cobra.Command, _ []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	ttl, _ := cmd.Flags().GetDuration(flagTTL)
	maxUses, _ := cmd.Flags().GetInt32(flagMaxUses)
	format, err := outputFormat(cmd, "text")
	if err != nil {
		return err
	}
	if ttl < time.Minute || ttl > 24*time.Hour {
		return fmt.Errorf("validation_failed: --ttl must be in [1m, 24h]")
	}

	kind := "cluster"
	req := cpclient.CreateJoinTokenRequest{Kind: &kind}
	ttlSeconds := int(ttl.Seconds())
	req.TTLSeconds = &ttlSeconds
	if maxUses != 0 {
		if maxUses < 0 {
			return fmt.Errorf("validation_failed: --max-uses must be >= 1")
		}
		v := maxUses
		req.MaxUses = &v
	}

	resp, err := c.CreateJoinToken(cmd.Context(), req)
	if err != nil {
		return classifyError(err)
	}

	switch format {
	case "json":
		raw, err := json.MarshalIndent(resp, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
	default:
		printClusterTokenBundle(cmd, resp, ttl)
	}
	return nil
}

// printClusterTokenBundle renders the bundle plus a ready-to-paste
// otherix-api join hint. The CP URL is unknown to the CLI (it dials via
// the configured server), so the hint uses a <this-cp> placeholder.
func printClusterTokenBundle(cmd *cobra.Command, resp cpclient.CreateJoinTokenResponse, ttl time.Duration) {
	printf(cmd, "Cluster join token created (id: %s):\n\n", resp.ID)
	printf(cmd, "  %s\n\n", resp.Token)
	printf(cmd, "CA fingerprint:\n\n")
	printf(cmd, "  sha256:%s\n\n", resp.CAFingerprintSHA256)
	printf(cmd, "Save BOTH NOW - server stores only the hash; plaintext cannot be retrieved.\n")
	printf(cmd, "On the new control-plane host run:\n\n")
	printf(cmd, "  sudo otherix-api join --cp-url https://<this-cp>:8080 \\\n")
	printf(cmd, "    --token %s \\\n", resp.Token)
	printf(cmd, "    --ca-fingerprint sha256:%s --name <unique-member-name>\n\n", resp.CAFingerprintSHA256)
	printf(cmd, "Expires: %s (TTL: %s).\n", resp.ExpiresAt, ttl)
}
